package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// Compile-time check: *pgxpool.Pool satisfies DBTX.
var _ DBTX = (*pgxpool.Pool)(nil)

// Compile-time interface assertions.
var (
	_ core.JournalWriter         = (*LedgerStore)(nil)
	_ core.TemplateBatchExecutor = (*LedgerStore)(nil)
	_ core.BalanceReader         = (*LedgerStore)(nil)
)

// LedgerStore implements JournalWriter and BalanceReader using PostgreSQL.
//
// In pool mode (constructed via NewLedgerStore), every write operation that
// requires atomicity starts its own transaction. GetBalance wraps its two
// queries in a REPEATABLE READ transaction to prevent phantom reads.
//
// In tx mode (constructed via NewLedgerStore then bound via withDB), the store
// participates in the caller's transaction. Write operations that previously
// started their own transaction now use the provided pgx.Tx directly (they do
// not call Commit/Rollback — the caller owns the transaction lifecycle).
// GetBalance does NOT start a REPEATABLE READ sub-transaction; the caller's
// transaction isolation level applies instead.
type LedgerStore struct {
	// pool is non-nil only in pool mode. It is used for BeginTx when an
	// explicit isolation level (e.g. REPEATABLE READ for GetBalance) is needed.
	// When nil, the store is tx-bound and must use db directly.
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
	dims *dimCache

	// attestor configures per-journal authorization signing (design doc
	// §7, P5). attestor == nil (the zero value from NewLedgerStore) means
	// signing is not configured at all -- PostJournal behaves exactly as it
	// did before P5 existed (expand-safe, design doc §12), and every
	// journal's auth_digest/auth_signature/auth_key_id stay empty. Set via
	// WithAuth, never by mutating a live store.
	attestor core.Attestor
}

// balancePair identifies a (holder, currency_id) pair targeted by an advisory
// lock. Used to dedupe + sort the entries in a journal before locking.
type balancePair struct {
	holder     int64
	currencyID int64
}

// balancePairsFromEntries returns the unique (holder, currency_id) pairs in
// entries, sorted lexicographically. Sorted order is required to take advisory
// locks in the same global order across concurrent transactions, otherwise
// deadlocks become possible (tx A locks pair P1 then P2 while tx B locks P2
// then P1).
// resolvedEntry is an EntryInput whose uid dimension references have been
// resolved to internal storage ids (plus the dimension metadata the write
// pipeline needs). It exists only inside the postgres adapter — internal ids
// never cross back into core types (api-contract §3).
type resolvedEntry struct {
	core.EntryInput
	currencyID       int64
	classificationID int64
	exponent         int32
	normalSide       core.NormalSide
}

// resolveEntries maps every entry's currency/classification uid to internal
// dimensions via the dims cache (one refresh at most for the whole batch).
func (s *LedgerStore) resolveEntries(ctx context.Context, q *sqlcgen.Queries, entries []core.EntryInput) ([]resolvedEntry, error) {
	out := make([]resolvedEntry, len(entries))
	for i, e := range entries {
		cur, err := s.dims.currencyByUIDOrErr(ctx, q, e.CurrencyUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
		cls, err := s.dims.classByUIDOrErr(ctx, q, e.ClassificationUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: entry[%d]: %w", i, err)
		}
		out[i] = resolvedEntry{
			EntryInput:       e,
			currencyID:       cur.ID,
			classificationID: cls.ID,
			exponent:         cur.Exponent,
			normalSide:       cls.NormalSide,
		}
	}
	return out, nil
}

func balancePairsFromEntries(entries []resolvedEntry) []balancePair {
	pairs := make([]balancePair, 0, len(entries))
	for _, e := range entries {
		pairs = append(pairs, balancePair{holder: e.AccountHolder, currencyID: e.currencyID})
	}
	return sortedUniquePairs(pairs)
}

// sortedUniquePairs dedupes pairs and sorts them lexicographically by
// (holder, currency_id). Extracted out of balancePairsFromEntries so a caller
// that needs the lock order for MORE than one journal's entries at once (see
// ExecuteTemplateBatch below) can union several journals' pairs first and
// still get the same canonical order a single-journal caller would derive —
// sorting per-journal and then concatenating would NOT give the same order
// two batches with a different journal sequence would need to agree on.
//
// The rule this function exists to serve, stated so it is greppable rather
// than remembered: ANY call site that takes balance locks outside
// postJournalWithQueries must lock the COMPLETE set of pairs its transaction
// will touch, through this function. Locking a subset "because that is the
// one I need to read" inverts the order for whatever runs next in the same
// transaction -- which is exactly how PendingStore came to hold the user's
// pair while PostJournal asked for the system counterpart's, against a whole
// repository that does the opposite (concurrency.md 2026-09-02). Advisory
// xact locks are re-entrant, so over-locking here costs nothing and the
// canonical order is preserved.
func sortedUniquePairs(pairs []balancePair) []balancePair {
	seen := make(map[balancePair]struct{}, len(pairs))
	out := make([]balancePair, 0, len(pairs))
	for _, p := range pairs {
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].holder != out[j].holder {
			return out[i].holder < out[j].holder
		}
		return out[i].currencyID < out[j].currencyID
	})
	return out
}

// acquireBalanceLocks takes a transaction-scoped advisory lock on every
// (holder, currency_id) pair in pairs. Pairs must be presorted (see
// balancePairsFromEntries). The locks are tx-scoped and released at COMMIT/ROLLBACK.
//
// pg_advisory_xact_lock blocks (it is not the _try_ variant), so this is
// exactly where a genuine ABBA deadlock between two transactions' lock
// orders would surface as SQLSTATE 40P01 -- routed through
// normalizeStoreError (bus #24) so it comes back wrapped in
// core.ErrTransient instead of an unclassified error only
// core.IsRetryable's default:true catch-all would call retryable.
func acquireBalanceLocks(ctx context.Context, q *sqlcgen.Queries, pairs []balancePair) error {
	for _, p := range pairs {
		key := fmt.Sprintf("balance:%d:%d", p.holder, p.currencyID)
		if err := q.AcquireBalanceLock(ctx, key); err != nil {
			return fmt.Errorf("postgres: post journal: advisory lock (%d,%d): %w", p.holder, p.currencyID, normalizeStoreError(err))
		}
	}
	return nil
}

// acquireIdempotencyLock takes a transaction-scoped advisory lock on key.
// Like acquireBalanceLocks, this blocks rather than trying, so a real
// deadlock across two transactions' lock orders surfaces here as SQLSTATE
// 40P01 -- routed through normalizeStoreError (bus #24) for the same reason.
func acquireIdempotencyLock(ctx context.Context, q *sqlcgen.Queries, key string) error {
	if err := q.AcquireIdempotencyLock(ctx, key); err != nil {
		return fmt.Errorf("postgres: idempotency lock %q: %w", key, normalizeStoreError(err))
	}
	return nil
}

// NewLedgerStore creates a new LedgerStore backed by a connection pool. The
// store starts its own transactions for write operations and uses REPEATABLE
// READ isolation for GetBalance.
func NewLedgerStore(pool *pgxpool.Pool) *LedgerStore {
	return &LedgerStore{
		pool: pool,
		db:   pool,
		q:    sqlcgen.New(pool),
		dims: dimCacheFor(pool),
	}
}

// WithDB returns a clone of the LedgerStore bound to an existing transaction
// (or any DBTX implementor). The clone shares no mutable state with the
// original and is safe for concurrent use alongside it. The caller owns the
// transaction lifecycle (commit/rollback).
//
// The clone never signs journals regardless of attestor (carried over here
// only for field-consistency, not because it is consulted): pool == nil is
// PostJournal's signal that it is running inside a transaction it did not
// open, and financial.md forbids the Attestor's signing call from
// happening there. See WithAuth's doc comment.
func (s *LedgerStore) WithDB(db DBTX) *LedgerStore {
	return &LedgerStore{
		pool:     nil, // tx mode: pool deliberately nil
		db:       db,
		q:        sqlcgen.New(db),
		dims:     s.dims,
		attestor: s.attestor,
	}
}

// WithAuth returns a clone of s configured to sign every journal through
// attestor (design doc §7, P5, as simplified by Team Lead 2026-08-21: no
// per-journal-type coverage decision, no failure-mode policy -- every
// posting is signed, and a Sign error simply propagates like any other
// error). Call it once, right after NewLedgerStore, before the store does
// any writes.
//
// attestor == nil (never calling WithAuth) is the supported, default state:
// the signing feature is entirely off, and every journal's auth_digest/
// auth_signature/auth_key_id stay empty, byte-for-byte identical to
// PostJournal's behavior before P5 existed. There is no separate
// "configured but broken" state to worry about here: attestor is
// constructed by the caller (e.g. authdev.NewLocalAttestor) BEFORE it is
// ever passed to WithAuth, so a bad key/seed fails at that construction
// call, in the caller's own composition root -- never silently inside this
// store.
func (s *LedgerStore) WithAuth(attestor core.Attestor) *LedgerStore {
	return &LedgerStore{
		pool:     s.pool,
		db:       s.db,
		q:        s.q,
		dims:     s.dims,
		attestor: attestor,
	}
}

// resolveEffectiveAt applies core.JournalInput.EffectiveAt's documented
// zero-value-means-now default. Pulled out to its own function so
// PostJournal can resolve it ONCE, before signing (see attestJournal), and
// thread the exact same value through to postJournalWithQueries -- two
// independent time.Now() calls a few lines apart would sign one instant and
// persist a different one, breaking the digest/row correspondence
// VerifyJournalAuth depends on.
func resolveEffectiveAt(t time.Time) time.Time {
	if t.IsZero() {
		return time.Now()
	}
	return t
}

// journalAuth is the (possibly empty) result of attestJournal, or the
// unpacked form of a caller-supplied core.AuthorizedJournal (PostAuthorized).
// status is never the Go zero value ("") on any path that reaches
// postJournalWithQueries -- every constructor site sets it explicitly (see
// core.AuthStatus's doc comment on why the zero value must never reach the
// DB unlabeled).
type journalAuth struct {
	digest    []byte
	signature []byte
	keyID     string
	status    core.AuthStatus
	// replay marks "this idempotency key already has a posted journal, so
	// nothing was signed and nothing is meant to be inserted"; status then
	// carries the ALREADY-STORED journal's auth_status, read back from that
	// row (attestJournal), never a value chosen to stand in for it.
	//
	// This flag exists because the replay case used to be labelled
	// core.AuthStatusUnsignedNoAttestor, which is not a placeholder: it is
	// the value service/attest_verify.go reads as "forged, or posted before
	// the signing key was wired" (tamper-evident.md m-6). Nothing kept that
	// mislabel out of the DB except one caller's locked recheck happening to
	// short-circuit first -- an invariant held by a comment, not by
	// structure. postJournalWithQueries now refuses outright to insert a row
	// from a replay-flagged auth, so the label can never be persisted by any
	// future write path either.
	replay bool
}

// bytesOrEmpty normalizes a nil slice to a non-nil empty one. auth_digest/
// auth_signature are BYTEA NOT NULL DEFAULT (empty bytea) (migration 046)
// -- passing a nil []byte through pgx would bind SQL NULL, violating that
// constraint, so every unsigned path must use []byte{}, never nil.
func bytesOrEmpty(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// attestJournal resolves whether and how to sign input before any
// transaction is opened -- financial.md forbids external calls inside a
// DB transaction, and this is the only place in the PostJournal call chain
// that runs strictly before one. Called from PostJournal's pool-mode branch
// (s.pool != nil) and from the public Authorize (design doc §7.5); the
// tx-mode branch never calls it at all (see PostJournal's doc comment).
//
// Returns journalAuth{status: core.AuthStatusUnsignedNoAttestor} (no error)
// when s.attestor is nil -- signing is not configured at all (design doc
// §12: expand-safe, behavior unchanged from before P5).
//
// Returns journalAuth{replay: true, status: <the stored row's auth_status>}
// (no error) when input.IdempotencyKey already has a posted journal: the
// stored journal's own signature is what VerifyJournalAuth will see when read
// back, and no new signing call happens (design doc §7.3, "same key + same
// payload -> digest same -> reuse, don't resign"). The status is READ from
// that row rather than invented, so a replay of a signed journal reports
// `signed`; it used to report unsigned_no_attestor, the value VerifyLedger
// reads as suspected forgery (tamper-evident.md m-6). This check is a
// best-effort optimization -- postJournalWithQueries's own locked recheck is
// what actually enforces idempotency -- but "the label does not matter
// because the insert never happens" is a property of that one caller, so
// postJournalWithQueries fails closed on a replay-flagged auth that somehow
// reaches its insert path instead of relying on it.
//
// Every journal is signed once an Attestor is configured -- there is no
// per-journal-type coverage decision (Team Lead's 2026-08-21
// simplification: the original per-type/failure-mode policy surface was
// solving a remote-KMS deployment problem this project does not have).
// A Sign error propagates as a plain wrapped error (errors as data,
// `discipline.md` §6) -- there is no fail-open branch that would let an
// unsigned journal post silently.
func (s *LedgerStore) attestJournal(ctx context.Context, input core.JournalInput, effectiveAt time.Time) (journalAuth, error) {
	if s.attestor == nil {
		return journalAuth{status: core.AuthStatusUnsignedNoAttestor}, nil
	}

	if existing, err := s.q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
		return journalAuth{replay: true, status: core.AuthStatus(existing.AuthStatus)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return journalAuth{}, fmt.Errorf("postgres: post journal: check idempotency before signing: %w", err)
	}

	digest, err := core.CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		return journalAuth{}, fmt.Errorf("postgres: post journal: canonical digest: %w", err)
	}

	signature, keyID, err := s.attestor.Sign(ctx, digest)
	if err != nil {
		return journalAuth{}, fmt.Errorf("postgres: post journal: attestor sign: %w: %w", err, core.ErrAttestorUnavailable)
	}
	return journalAuth{digest: digest, signature: signature, keyID: keyID, status: core.AuthStatusSigned}, nil
}

// Authorize computes input's canonical digest and, if an Attestor is
// configured, signs it -- entirely outside any DB transaction, so callers
// can safely wrap the result (PostAuthorized) inside a RunInTx callback
// (design doc §7.5). It is the ONLY safe way to get a signature onto a
// journal that must be composed with a caller-owned transaction; calling
// PostJournal itself from tx mode never signs (see PostJournal's doc
// comment).
//
// Returns an error if called on a store already bound to a transaction
// (s.pool == nil -- i.e. a *LedgerStore obtained via WithDB, such as the
// clone RunInTx hands to its callback): that would mean a transaction is
// already open, and calling out to an Attestor from inside one violates
// financial.md exactly as PostJournal's tx-mode branch was built to avoid.
// Authorize must run BEFORE RunInTx opens, on the top-level store/Service.
//
// input.EventUID does not need to be known yet (e.g. it will be minted by
// a booking transition inside the transaction this pre-authorization is
// for): CanonicalJournalDigest never covers it, so setting it later on
// AuthorizedJournal.Input does not invalidate the signature -- see
// AuthorizedJournal's doc comment.
func (s *LedgerStore) Authorize(ctx context.Context, input core.JournalInput) (core.AuthorizedJournal, error) {
	if s.pool == nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize: called on a transaction-bound store; Authorize must run before opening a transaction, not from inside RunInTx: %w", core.ErrInvalidInput)
	}
	if err := input.Validate(); err != nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize: %w", err)
	}

	effectiveAt := resolveEffectiveAt(input.EffectiveAt)
	auth, err := s.attestJournal(ctx, input, effectiveAt)
	if err != nil {
		return core.AuthorizedJournal{}, err
	}
	return core.AuthorizedJournal{
		Input:       input,
		EffectiveAt: effectiveAt,
		Digest:      auth.digest,
		Signature:   auth.signature,
		KeyID:       auth.keyID,
		Status:      auth.status,
	}, nil
}

// PostAuthorized posts authorized.Input using the signature Authorize
// already computed -- it NEVER calls the Attestor, regardless of pool or
// tx mode, which is what makes it safe to call from inside a RunInTx
// callback (design doc §7.5). authorized.Status must not be empty (the
// zero value of core.AuthStatus): that would mean authorized was not built
// by Authorize, which this method treats as a caller bug, not a value to
// paper over with a guessed status.
func (s *LedgerStore) PostAuthorized(ctx context.Context, authorized core.AuthorizedJournal) (*core.Journal, error) {
	if authorized.Status == "" {
		return nil, fmt.Errorf("postgres: post authorized: AuthorizedJournal.Status is empty; it must come from Authorize, not be constructed by hand: %w", core.ErrInvalidInput)
	}
	if err := authorized.Input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: post authorized: %w", err)
	}
	auth := journalAuth{
		digest:    authorized.Digest,
		signature: authorized.Signature,
		keyID:     authorized.KeyID,
		status:    authorized.Status,
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly -- exactly the
		// RunInTx case this method exists for.
		return s.postJournalWithQueries(ctx, s.q, authorized.Input, authorized.EffectiveAt, auth)
	}

	// Pool mode: own the transaction lifecycle. No Attestor call happens
	// here either -- authorized already carries whatever Authorize decided.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: post authorized: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.postJournalWithQueries(ctx, qtx, authorized.Input, authorized.EffectiveAt, auth)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: post authorized: commit: %w", err)
	}
	return journal, nil
}

// PostJournal posts a balanced journal within a transaction.
// Idempotent: same key + same payload returns the existing journal; divergent
// payload returns ErrConflict.
//
// In pool mode a new transaction is started and committed here -- and, if
// an Attestor is configured (WithAuth), this is also the only place
// signing happens: attestJournal runs to completion, including the
// Attestor's KMS call, strictly before pool.Begin (financial.md: no
// external calls inside a DB transaction).
//
// In tx mode (store bound via WithDB, i.e. inside ledger.Service.RunInTx or
// any other caller-owned transaction) the journal is written directly into
// the caller's transaction; commit/rollback is the caller's responsibility.
// Signing is deliberately NOT attempted in this mode, regardless of
// whether an Attestor is configured: the transaction already exists and
// was opened by someone else, so there is no safe point left at which to
// call out to KMS without violating financial.md. Journals posted this way
// keep auth_digest/auth_signature/auth_key_id empty -- indistinguishable
// from the "no Attestor configured" case until a downstream consumer
// (design doc §7.3/§7.4, not wired in this phase) decides to care.
func (s *LedgerStore) PostJournal(ctx context.Context, input core.JournalInput) (*core.Journal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.post_journal",
		attribute.String("idempotency_key", input.IdempotencyKey),
		attribute.String("journal_type_uid", input.JournalTypeUID),
		attribute.Int64("actor_id", input.ActorID),
		attribute.String("source", input.Source),
	)
	defer span.End()

	if err := input.Validate(); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly. Never signs -- see
		// doc comment above. Callers that need a signature on a journal
		// composed via RunInTx must use Authorize + PostAuthorized instead
		// (design doc §7.5) -- this branch has no way to tell "the caller
		// deliberately skipped signing" apart from "the caller never
		// thought about it", which is exactly why it is always labeled
		// unsigned_tx_mode rather than guessing.
		effectiveAt := resolveEffectiveAt(input.EffectiveAt)
		j, err := s.postJournalWithQueries(ctx, s.q, input, effectiveAt, journalAuth{status: core.AuthStatusUnsignedTxMode})
		ledgerotel.RecordError(span, err)
		return j, err
	}

	// Pool mode: this call owns its transaction, so it is the one place
	// signing can safely happen. Resolve EffectiveAt once here so the exact
	// same instant is both signed (attestJournal) and persisted
	// (postJournalWithQueries) -- see resolveEffectiveAt's doc comment.
	effectiveAt := resolveEffectiveAt(input.EffectiveAt)
	auth, err := s.attestJournal(ctx, input, effectiveAt)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: post journal: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.postJournalWithQueries(ctx, qtx, input, effectiveAt, auth)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: post journal: commit: %w", err)
	}

	return journal, nil
}

// RenderTemplate loads a template by code and renders it into a
// core.JournalInput without posting it. Read-only (template + dimension
// lookups only, no writes), so it is safe to call in any mode -- but it
// exists specifically so a caller can render a template-driven journal
// BEFORE opening a transaction, then pass the result to Authorize and
// finally PostAuthorized inside RunInTx (design doc §7.5). ExecuteTemplate
// remains the one-call convenience path for callers that do not need to
// sign across a RunInTx boundary.
func (s *LedgerStore) RenderTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (*core.JournalInput, error) {
	return s.renderTemplate(ctx, s.q, templateCode, params)
}

// ExecuteTemplate loads a template by code, renders it, and posts the journal.
func (s *LedgerStore) ExecuteTemplate(ctx context.Context, templateCode string, params core.TemplateParams) (*core.Journal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.execute_template",
		attribute.String("template_code", templateCode),
	)
	defer span.End()

	input, err := s.renderTemplate(ctx, s.q, templateCode, params)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	j, err := s.PostJournal(ctx, *input)
	ledgerotel.RecordError(span, err)
	return j, err
}

// ExecuteTemplateBatch renders and posts multiple templates in a single transaction.
//
// In pool mode a new transaction is started and committed here (all-or-nothing).
// In tx mode (store bound via withDB) all journals are written directly into
// the caller's transaction; commit/rollback is the caller's responsibility.
//
// Pool mode with an Attestor configured renders every template AND signs
// every resulting input strictly BEFORE pool.Begin (board #15, W2-T1,
// extending design doc §7.2/§7.5's "sign outside any transaction" rule to
// batches -- previously out of scope, see the retired doc comment on
// executeTemplateBatchWithQueries this replaces). Rendering earlier does not
// weaken the "all-or-nothing" write guarantee: every INSERT still happens
// inside the one transaction opened below, unchanged; only the read-only
// template lookup moves earlier, and postJournalWithQueries persists
// whatever `core.JournalInput` it is given regardless of when that value was
// produced -- the same principle Authorize/PostAuthorized already rely on
// for a single journal.
//
// Both modes take the batch's locks up front, in one canonical order, via
// preacquireBatchLocks (see its doc comment). Signing is what differs between
// the two branches; lock order is not, and must not be -- a consumer reaching
// the tx branch through RunInTx deadlocks against a pool-mode batch just as
// readily as two pool-mode batches deadlock against each other.
func (s *LedgerStore) ExecuteTemplateBatch(ctx context.Context, requests []core.TemplateExecutionRequest) ([]*core.Journal, error) {
	if len(requests) == 0 {
		return nil, nil
	}

	if s.pool == nil {
		// Tx mode: write directly into caller's transaction. Never signs --
		// there is no point in this call chain provably outside a
		// transaction someone else opened (financial.md forbids calling the
		// Attestor from inside one).
		return s.executeTemplateBatchWithQueries(ctx, s.q, requests)
	}

	// Pool mode: render + sign every input before opening the transaction.
	inputs := make([]core.JournalInput, len(requests))
	effectiveAts := make([]time.Time, len(requests))
	auths := make([]journalAuth, len(requests))
	for i, req := range requests {
		input, err := s.renderTemplate(ctx, s.q, req.TemplateCode, req.Params)
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		effectiveAt := resolveEffectiveAt(input.EffectiveAt)
		auth, err := s.attestJournal(ctx, *input, effectiveAt)
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		inputs[i] = *input
		effectiveAts[i] = effectiveAt
		auths[i] = auth
	}

	// Own the transaction lifecycle. No Attestor call happens from here on --
	// every auths[i] was already decided above.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template batch: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	if err := s.preacquireBatchLocks(ctx, qtx, inputs); err != nil {
		return nil, err
	}

	journals := make([]*core.Journal, 0, len(inputs))
	for i, input := range inputs {
		journal, err := s.postJournalWithQueries(ctx, qtx, input, effectiveAts[i], auths[i])
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		journals = append(journals, journal)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: execute template batch: commit: %w", err)
	}

	return journals, nil
}

// preacquireBatchLocks fixes the whole batch's lock order at the
// transaction's first locking statement, before any journal is posted. Both
// ExecuteTemplateBatch branches call it -- the pool-mode one above and the
// tx-mode executeTemplateBatchWithQueries below -- because the deadlock it
// prevents does not care which of the two opened the transaction
// (concurrency.md 2026-09-02 Major: the fix had landed on the pool branch
// only, and `RunInTx` + `tx.TemplateBatchExecutor()` is an ordinary way for a
// consumer to reach the other one).
//
// Balance locks: balancePairsFromEntries only orders locks WITHIN a single
// journal, but a batch posts N journals in one transaction. Two concurrent
// batches whose journals touch the same holders in a different sequence
// could each acquire their first journal's balance lock and then block
// waiting for the other's -- an ABBA deadlock that needs no malicious input,
// just two ordinary batches (e.g. two batch settlements) that happen to list
// the same two holders in reverse order. Pre-acquiring the union of every
// pair the batch will touch, sorted once, fixes the order instead of
// re-deriving it per journal. Each journal's own acquireBalanceLocks call
// inside postJournalWithQueries then re-takes the same (already-held) locks
// -- a no-op under Postgres's reentrant advisory xact locks -- so a
// lone-journal caller (ExecuteTemplate, which never goes through this batch
// path) is unaffected.
//
// Lock ORDER across lock kinds must also match the single-journal path,
// which takes idempotency → balance (postJournalWithQueries; Reserve is the
// same). This batch used to enter balance locks first and re-take each
// journal's idempotency lock later — the reverse. A concurrent single-journal
// retry of one of this batch's keys could then hold idem:K while waiting for
// bal:H held here, while this transaction held bal:H waiting for idem:K:
// ABBA, SQLSTATE 40P01. Postgres resolves it (ErrTransient, retry succeeds),
// so it was noise rather than corruption — but one canonical order costs
// nothing. Keys are sorted so two batches sharing keys agree with each other
// too.
func (s *LedgerStore) preacquireBatchLocks(ctx context.Context, q *sqlcgen.Queries, inputs []core.JournalInput) error {
	idemKeys := make([]string, 0, len(inputs))
	seenIdemKeys := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if _, ok := seenIdemKeys[input.IdempotencyKey]; ok {
			continue
		}
		seenIdemKeys[input.IdempotencyKey] = struct{}{}
		idemKeys = append(idemKeys, input.IdempotencyKey)
	}
	sort.Strings(idemKeys)
	for _, key := range idemKeys {
		if err := acquireIdempotencyLock(ctx, q, key); err != nil {
			return fmt.Errorf("postgres: execute template batch: %w", err)
		}
	}

	var allPairs []balancePair
	for i, input := range inputs {
		resolved, err := s.resolveEntries(ctx, q, input.Entries)
		if err != nil {
			return fmt.Errorf("postgres: execute template batch[%d]: resolve entries for lock order: %w", i, err)
		}
		allPairs = append(allPairs, balancePairsFromEntries(resolved)...)
	}
	if err := acquireBalanceLocks(ctx, q, sortedUniquePairs(allPairs)); err != nil {
		return fmt.Errorf("postgres: execute template batch: %w", err)
	}
	return nil
}

// executeTemplateBatchWithQueries is the tx-mode-only path: q is always the
// caller's own transaction (ExecuteTemplateBatch's s.pool == nil branch), so
// this always runs inside a transaction this code did not open and has no
// safe point to call the Attestor from without violating financial.md --
// every journal is unconditionally journalAuth{status:
// core.AuthStatusUnsignedTxMode}. Pool mode no longer calls this function
// (see ExecuteTemplateBatch's doc comment for why it was able to stop
// sharing this code path once signing needed to move before pool.Begin).
func (s *LedgerStore) executeTemplateBatchWithQueries(ctx context.Context, q *sqlcgen.Queries, requests []core.TemplateExecutionRequest) ([]*core.Journal, error) {
	inputs := make([]core.JournalInput, len(requests))
	for i, req := range requests {
		input, err := s.renderTemplate(ctx, q, req.TemplateCode, req.Params)
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		inputs[i] = *input
	}

	if err := s.preacquireBatchLocks(ctx, q, inputs); err != nil {
		return nil, err
	}

	journals := make([]*core.Journal, 0, len(inputs))
	for i, input := range inputs {
		effectiveAt := resolveEffectiveAt(input.EffectiveAt)
		journal, err := s.postJournalWithQueries(ctx, q, input, effectiveAt, journalAuth{status: core.AuthStatusUnsignedTxMode})
		if err != nil {
			return nil, fmt.Errorf("postgres: execute template batch[%d]: %w", i, err)
		}
		journals = append(journals, journal)
	}
	return journals, nil
}

// ReverseJournal creates a full reversal journal for the given journal ID.
// It rejects (ErrConflict) if journalID already has any reversal recorded
// against it, full or partial — see ReverseJournalFraction for posting
// additional partial reversals against a journal that already has history.
//
// In pool mode a new transaction is started and committed here: the
// SELECT ... FOR UPDATE row lock on the original journal and the reversal
// insert must share one transaction, so the "no reversal history yet" check
// cannot race a concurrent full or partial reversal. Migration 029 dropped
// the at-most-once unique index on reversal_of — this row lock is the only
// thing standing between two concurrent full reversals (with different
// reasons, hence different idempotency keys) and a 200% reversal. In tx
// mode (store bound via WithDB) it participates in the caller's transaction.
//
// Pool mode with an Attestor configured additionally pre-authorizes the
// reversal via AuthorizeReversal(num=1, den=1), strictly before pool.Begin
// (board #15, W2-T1). AuthorizeReversal's own idempotencyKey parameter is
// fed exactly this method's derived expectedKey (below) so the signed
// intent and the eventual post agree on which journal row an idempotent
// replay would find.
func (s *LedgerStore) ReverseJournal(ctx context.Context, journalUID string, reason string) (*core.Journal, error) {
	// The derived idempotency key stays keyed on the journal's uid so it is
	// stable across replays and never mentions the internal id. Computed
	// here (not inside reverseJournalWithQueries) so the pool-mode branch
	// below can pass the identical string into AuthorizeReversal before any
	// transaction opens.
	expectedKey := fmt.Sprintf("reversal:%s:%s", journalUID, reason)

	if s.pool == nil {
		return s.reverseJournalWithQueries(ctx, s.q, journalUID, reason, expectedKey, nil, journalAuth{status: core.AuthStatusUnsignedTxMode})
	}

	var preAuth *core.AuthorizedJournal
	fallback := journalAuth{status: core.AuthStatusUnsignedNoAttestor}
	if s.attestor != nil {
		authorized, err := s.AuthorizeReversal(ctx, journalUID, 1, 1, reason, expectedKey)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal: %w", err)
		}
		preAuth = &authorized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.reverseJournalWithQueries(ctx, qtx, journalUID, reason, expectedKey, preAuth, fallback)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: commit: %w", err)
	}
	return journal, nil
}

// reverseJournalWithQueries does the actual read-lock-write. preAuth and
// fallback carry the same meaning as reverseJournalFractionWithQueries's
// parameters of the same name (see that function's doc comment): preAuth,
// when non-nil, is compared by digest against the freshly-derived entries
// below (under the row lock ListReversalsByOriginalJournalID and
// GetJournalForUpdateByUID together provide) and used verbatim on a match,
// or rejected outright on a mismatch (a concurrent reversal landed in
// between); fallback is used whenever preAuth is nil.
func (s *LedgerStore) reverseJournalWithQueries(ctx context.Context, q *sqlcgen.Queries, journalUID string, reason string, expectedKey string, preAuth *core.AuthorizedJournal, fallback journalAuth) (*core.Journal, error) {
	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return nil, err
	}
	// Row-lock the original journal for the rest of this transaction, same as
	// ReverseJournalFraction: full and partial reversals of one journal all
	// serialize on this lock, so the history check below sees every committed
	// reversal and no concurrent one can land until we commit.
	original, err := q.GetJournalForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reverse journal: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: reverse journal: get journal: %w", err)
	}
	journalID := original.ID
	if original.ReversalOf.Valid {
		return nil, fmt.Errorf("postgres: reverse journal: journal %q is already a reversal: %w", journalUID, core.ErrConflict)
	}

	existingReversals, err := q.ListReversalsByOriginalJournalID(ctx, int64ToInt8(&journalID))
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: lookup reversals: %w", err)
	}
	if len(existingReversals) > 0 {
		// Same (journal, reason) as an existing reversal → idempotent retry,
		// return it. Any other existing reversal — full or partial, any
		// reason — means this journal already has reversal history; a second
		// full reversal on top of that would double-count whatever was
		// already reversed, so it is rejected regardless of reason text.
		for _, r := range existingReversals {
			if r.IdempotencyKey == expectedKey {
				return journalFromRow(ctx, s.dims, q, r)
			}
		}
		return nil, fmt.Errorf(
			"postgres: reverse journal: journal %q already has %d reversal(s) recorded; use ReverseJournalFraction for further partial reversals: %w",
			journalUID, len(existingReversals), core.ErrConflict,
		)
	}

	entries, err := q.ListJournalEntries(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: list entries: %w", err)
	}

	// The existingReversals check above already guarantees zero prior
	// reversals reach this point, so reversalEntriesFor's num==den branch
	// with an empty alreadyReversed map reduces to exactly "flip every
	// original entry at its full original amount" — the same computation
	// AuthorizeReversal ran, unlocked, before this transaction opened (see
	// its doc comment).
	reversedEntries, err := s.reversalEntriesFor(ctx, q, entries, map[entryDimKey]decimal.Decimal{}, 1, 1)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: %w", err)
	}

	jt, err := s.dims.jtByIDOrErr(ctx, q, original.JournalTypeID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal: %w", err)
	}
	input := core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: expectedKey,
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOfUID:  journalUID,
		Metadata:       map[string]string{"reason": reason},
	}

	effectiveAt := resolveEffectiveAt(input.EffectiveAt)
	auth := fallback
	if preAuth != nil {
		// Reuse the exact instant AuthorizeReversal signed -- see
		// reverseJournalFractionWithQueries's identical comment for why a
		// fresh resolveEffectiveAt here would break the comparison even
		// with zero concurrent state change.
		effectiveAt = preAuth.EffectiveAt
		digest, err := core.CanonicalJournalDigest(input, effectiveAt)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal: recompute digest: %w", err)
		}
		if !bytes.Equal(digest, preAuth.Digest) {
			// existingReversals == 0 was just proven above under this row's
			// lock, and the general (non-num==den) overshoot path does not
			// apply to a full reversal, so this branch is not expected to
			// be reachable in practice -- kept as a fail-closed backstop
			// rather than a panic, consistent with working-agreements §3.
			return nil, fmt.Errorf(
				"postgres: reverse journal: reversal intent changed since AuthorizeReversal ran for journal %q; retry: %w",
				journalUID, core.ErrConflict,
			)
		}
		auth = journalAuth{digest: preAuth.Digest, signature: preAuth.Signature, keyID: preAuth.KeyID, status: preAuth.Status}
	}
	return s.postJournalWithQueries(ctx, q, input, effectiveAt, auth)
}

func (s *LedgerStore) renderTemplate(ctx context.Context, q *sqlcgen.Queries, templateCode string, params core.TemplateParams) (*core.JournalInput, error) {
	tmplRow, err := q.GetTemplateByCode(ctx, templateCode)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: execute template: template %q: %w", templateCode, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: execute template: get template: %w", err)
	}

	lines, err := q.GetTemplateLines(ctx, tmplRow.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: get lines: %w", err)
	}

	tmpl, err := templateFromRow(ctx, s.dims, q, tmplRow, lines)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: %w", err)
	}
	input, err := tmpl.Render(params)
	if err != nil {
		return nil, fmt.Errorf("postgres: execute template: render: %w", err)
	}
	return input, nil
}

// postJournalWithQueries writes input using the already-resolved
// effectiveAt (see resolveEffectiveAt -- callers must not let this function
// re-resolve "now" independently, or a signed digest and its persisted row
// could disagree on the timestamp) and auth (the result of attestJournal or
// an unpacked core.AuthorizedJournal from PostAuthorized). auth.status is
// persisted verbatim as journals.auth_status and must never be the Go zero
// value ("") on this path -- every caller sets it explicitly (see
// journalAuth's doc comment).
func (s *LedgerStore) postJournalWithQueries(ctx context.Context, q *sqlcgen.Queries, input core.JournalInput, effectiveAt time.Time, auth journalAuth) (*core.Journal, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}

	existing, err := q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency: %w", err)
	}

	if err := acquireIdempotencyLock(ctx, q, input.IdempotencyKey); err != nil {
		return nil, err
	}

	existing, err = q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureJournalMatchesInput(ctx, q, existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: check idempotency after lock: %w", err)
	}

	// Resolve every uid reference to internal storage ids up front: entry
	// dimensions, journal type, and the optional event/reversal links. This is
	// the single boundary where uid-space input becomes id-space storage.
	resolved, err := s.resolveEntries(ctx, q, input.Entries)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	jt, err := s.dims.jtByUIDOrErr(ctx, q, input.JournalTypeUID)
	if err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	eventID := int64(0)
	if input.EventUID != "" {
		pgUID, err := uidToPG(input.EventUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: post journal: event %q: %w", input.EventUID, core.ErrNotFound)
		}
		event, err := q.GetEventByUID(ctx, pgUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("postgres: post journal: event %q: %w", input.EventUID, core.ErrNotFound)
			}
			return nil, fmt.Errorf("postgres: post journal: resolve event: %w", err)
		}
		eventID = event.ID
		// Set-once, checked at the gate (I-51, rule 4). This was previously
		// only caught by linkJournalToEventAndBooking, at the very end of
		// this function -- after the journal and every entry had been
		// inserted and the whole transaction had to unwind. More importantly,
		// checking it here is what makes the dimension rule below meaningful:
		// both halves of "may this journal claim this event" are decided in
		// one place, before anything is written.
		if event.JournalID.Valid {
			return nil, fmt.Errorf(
				"postgres: post journal: event %q is already linked to a journal: %w",
				input.EventUID, core.ErrConflict,
			)
		}

		// The event's own dimension, used when the event carries no booking.
		// No writer produces such an event today (Booker.Transition is the
		// only INSERT site and always copies its booking's holder/currency),
		// so this is the defensive branch, not the normal one.
		linkHolder, linkCurrencyID := event.AccountHolder, event.CurrencyID
		if event.BookingID != 0 {
			// Take the booking's row lock HERE, before acquireBalanceLocks
			// below (concurrency.md 2026-09-02 Minor). linkJournalToEventAndBooking
			// UPDATEs this row at the end of this function, which takes the
			// same lock implicitly -- but by then the balance locks are
			// already held, giving this path the order
			// balance -> booking row, while Booker.Transition (and every
			// caller following CLAUDE.md's Event-Journal atomicity recipe,
			// including this library's own deposit confirmation) has the
			// order booking row -> balance. That is a cycle between two
			// perfectly ordinary calls, and event_uid is a wire field on
			// POST /journals, so a consumer can reach it without ever
			// touching Go. Locking here makes both paths
			// booking row -> balance; re-entrant for the Transition path,
			// which already holds this row's lock in the same transaction.
			booking, err := q.GetBookingForUpdate(ctx, event.BookingID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, fmt.Errorf("postgres: post journal: booking %d from event %q: %w", event.BookingID, input.EventUID, core.ErrNotFound)
				}
				return nil, fmt.Errorf("postgres: post journal: lock booking %d: %w", event.BookingID, normalizeStoreError(err))
			}
			// The booking is authoritative for the dimension; the event's own
			// copy is written from it.
			linkHolder, linkCurrencyID = booking.AccountHolder, booking.CurrencyID
		}

		// This journal must actually be about the thing the event happened to
		// (I-51, rule 4). event_uid was previously an existence check only,
		// and the link it creates is consumed as a semantic fact: it fills
		// events.journal_id and, through it, the booking's SET-ONCE
		// journal_id. So a journal touching nobody related to that booking
		// could claim it, and the booking's real settling transition would
		// then fail with ErrConflict forever -- with the wrong journal
		// standing as its accounting record. Requiring the booking's
		// (account_holder, currency) to appear among this journal's entries
		// is the weakest rule that makes the claim mean something; it does
		// not constrain amounts or classifications, which legitimately vary
		// (fees, spreads, multi-leg settlements).
		if linkHolder == 0 || linkCurrencyID == 0 {
			return nil, fmt.Errorf(
				"postgres: post journal: event %q has no account dimension to link against: %w",
				input.EventUID, core.ErrInvalidInput,
			)
		}
		if !slices.Contains(balancePairsFromEntries(resolved), balancePair{holder: linkHolder, currencyID: linkCurrencyID}) {
			return nil, fmt.Errorf(
				"postgres: post journal: event %q belongs to (holder %d, currency %d), which none of this journal's entries touch: %w",
				input.EventUID, linkHolder, linkCurrencyID, core.ErrInvalidInput,
			)
		}
	}
	if err := validateEntriesPrecision(ctx, s.dims, q, resolved); err != nil {
		return nil, err
	}
	// Soft-deleted dimensions are refused here, at the one choke point every
	// journal passes through (B-X1). is_active is the only mutable column on
	// the config tables and is deliberately not cached, so this is a read
	// rather than a cache consult; see assertDimsActive.
	if err := assertDimsActive(ctx, q, jt.ID, resolved); err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	reversalOfID := int64(0)
	if input.ReversalOfUID != "" {
		pgUID, err := uidToPG(input.ReversalOfUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: post journal: reversal_of %q: %w", input.ReversalOfUID, core.ErrNotFound)
		}
		// FOR UPDATE, not a plain read (I-51): validateReversalOfInput below
		// reads the referenced journal's entries AND its existing reversal
		// history, and both must stay put until this journal commits --
		// exactly the row lock ReverseJournal and
		// reverseJournalFractionWithQueries take, for exactly the same
		// reason. Taken HERE, before acquireBalanceLocks further down, so
		// both ways of posting a reversal agree on one lock order
		// (journal row -> balance advisory locks). Re-entrant when this call
		// came FROM one of those methods: they already hold this row's lock
		// in the same transaction, so this is a no-op for them and the
		// validation below simply re-runs at the choke point every reversal
		// passes through.
		orig, err := q.GetJournalForUpdateByUID(ctx, pgUID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("postgres: post journal: reversal_of %q: %w", input.ReversalOfUID, core.ErrNotFound)
			}
			return nil, fmt.Errorf("postgres: post journal: resolve reversal_of: %w", err)
		}
		if err := validateReversalOfInput(ctx, q, orig, resolved); err != nil {
			return nil, err
		}
		reversalOfID = orig.ID
	}

	// Period close (I-15, I-61): reject postings whose effective date falls
	// before the active close line. GetActivePeriodClose returns
	// pgx.ErrNoRows when the period has never been closed — nothing to
	// enforce in that case.
	//
	// The SHARED period barrier is taken FIRST and held until this
	// transaction ends. Without it this gate was a plain READ COMMITTED read:
	// ClosePeriod took no lock at all, so it could INSERT and COMMIT a new
	// line at any point between this read and this transaction's COMMIT, and
	// the journal landed behind a line that was already active. Reading the
	// line "in the same transaction as the write" (I-15's original Enforced
	// by) is not exclusion. Order-free by construction — the exclusive half
	// polls instead of queueing — so every write path funnelling through
	// here inherits the barrier without having to reason about where it sits
	// among its own locks (see queries/periods.sql).
	if err := acquirePeriodReadBarrier(ctx, q); err != nil {
		return nil, fmt.Errorf("postgres: post journal: %w", err)
	}
	activeClose, err := q.GetActivePeriodClose(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: post journal: get active period close: %w", err)
	}
	if err == nil && effectiveAt.Before(activeClose.CloseBefore) {
		return nil, fmt.Errorf(
			"postgres: post journal: effective_at %s is before the period close line %s: %w",
			effectiveAt.Format(time.RFC3339), activeClose.CloseBefore.Format(time.RFC3339), core.ErrPeriodClosed,
		)
	}

	// Invariant: every balance-mutating tx must take pg_advisory_xact_lock(holder,
	// currency_id) for every affected (holder, currency_id) pair, in sorted order.
	// This serializes against ReserverStore.Reserve (which takes the same lock),
	// preventing TOCTOU races where a reserve reads stale balance while a journal
	// is being committed. Locks are taken in lexicographic (holder, currency_id)
	// order to avoid deadlocks when two journals touch overlapping pairs.
	if err := acquireBalanceLocks(ctx, q, balancePairsFromEntries(resolved)); err != nil {
		return nil, err
	}

	// Account policy enforcement (I-17): frozen/closed status + min_balance,
	// evaluated inside the same advisory lock so it is TOCTOU-safe against
	// concurrent journals/reserves/policy changes on the same (holder,
	// currency) pairs. Must run before any row below is written since a
	// rejection here must abort the whole journal.
	if err := s.enforceAccountPolicies(ctx, q, resolved); err != nil {
		return nil, err
	}

	debit, credit := input.Totals()

	// Fail loudly on a caller bug rather than let an unlabeled row reach
	// the DB: every call site in this package sets auth.status explicitly
	// (journalAuth's doc comment), so an empty value here means a new
	// caller was added without doing so. The auth_status CHECK constraint
	// (migration 051) would also reject it, but that error is far less
	// legible than this one.
	if auth.status == "" {
		return nil, fmt.Errorf("postgres: post journal: internal: journalAuth.status is empty -- every caller of postJournalWithQueries must set it: %w", core.ErrInvalidInput)
	}
	// A replay-flagged auth means attestJournal found this key already
	// posted and deliberately signed nothing. Reaching here means the two
	// locked rechecks above did NOT find that journal, so inserting now
	// would write an unsigned row under a key that is supposed to already
	// resolve to a signed one. There is no correct row to write, so write
	// none (working-agreements §3: fail closed, never fall back to a label
	// that happens to be accepted).
	if auth.replay {
		return nil, fmt.Errorf(
			"postgres: post journal: internal: idempotency key %q was reported as an already-posted replay but the locked recheck did not find that journal; refusing to insert an unsigned row in its place: %w",
			input.IdempotencyKey, core.ErrConflict,
		)
	}

	row, err := q.InsertJournal(ctx, sqlcgen.InsertJournalParams{
		JournalTypeID:  jt.ID,
		IdempotencyKey: input.IdempotencyKey,
		TotalDebit:     decimalToNumeric(debit),
		TotalCredit:    decimalToNumeric(credit),
		Metadata:       metadataToJSON(input.Metadata),
		ActorID:        input.ActorID,
		Source:         input.Source,
		ReversalOf:     int64ToInt8(zeroInt64ToNil(reversalOfID)),
		EventID:        int64ToInt8(zeroInt64ToNil(eventID)),
		EffectiveAt:    effectiveAt,
		Uid:            newUID(),
		AuthDigest:     bytesOrEmpty(auth.digest),
		AuthSignature:  bytesOrEmpty(auth.signature),
		AuthKeyID:      auth.keyID,
		AuthStatus:     string(auth.status),
	})
	if err != nil {
		existing, lookupErr := q.GetJournalByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return s.ensureJournalMatchesInput(ctx, q, existing, input)
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: post journal: insert journal: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		}
		return nil, wrapStoreError("postgres: post journal: insert journal", err)
	}

	type rollupKey struct {
		holder           int64
		currencyID       int64
		classificationID int64
	}
	seen := make(map[rollupKey]struct{})

	for i, e := range resolved {
		_, err := q.InsertJournalEntry(ctx, sqlcgen.InsertJournalEntryParams{
			JournalID:        row.ID,
			AccountHolder:    e.AccountHolder,
			CurrencyID:       e.currencyID,
			ClassificationID: e.classificationID,
			EntryType:        string(e.EntryType),
			Amount:           decimalToNumeric(e.Amount),
			EffectiveAt:      effectiveAt,
		})
		if err != nil {
			return nil, wrapStoreError(fmt.Sprintf("postgres: post journal: insert entry[%d]", i), err)
		}

		key := rollupKey{holder: e.AccountHolder, currencyID: e.currencyID, classificationID: e.classificationID}
		seen[key] = struct{}{}
	}

	for key := range seen {
		if err := q.EnqueueRollup(ctx, sqlcgen.EnqueueRollupParams{
			AccountHolder:    key.holder,
			CurrencyID:       key.currencyID,
			ClassificationID: key.classificationID,
		}); err != nil {
			return nil, wrapStoreError("postgres: post journal: enqueue rollup", err)
		}
	}

	// Per-currency balance check: one query per posted journal, in the same
	// transaction, so a failure rolls back the journal and entries together
	// and the caller gets a precise "which currency" error. This is the
	// application-layer half of the defense; the DB-layer deferred
	// constraint trigger restored in migration 044 is the backstop for
	// direct SQL / a compromised app credential that bypasses this call
	// entirely (see docs/plans/2026-08-21-tamper-evident-ledger-design.md C1).
	badCurrency, err := q.VerifyJournalBalanced(ctx, row.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, wrapStoreError("postgres: post journal: verify balanced", err)
	}
	if err == nil {
		return nil, fmt.Errorf("postgres: post journal: journal %d unbalanced in currency %d: %w", row.ID, badCurrency, core.ErrUnbalancedJournal)
	}

	if eventID != 0 {
		if err := s.linkJournalToEventAndBooking(ctx, q, eventID, row.ID); err != nil {
			return nil, err
		}
	}

	return journalFromRow(ctx, s.dims, q, row)
}

func (s *LedgerStore) linkJournalToEventAndBooking(ctx context.Context, q *sqlcgen.Queries, eventID, journalID int64) error {
	event, err := q.GetEvent(ctx, eventID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: post journal: event %d: %w", eventID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: post journal: get event %d: %w", eventID, err)
	}
	if event.JournalID.Valid && event.JournalID.Int64 != journalID {
		return fmt.Errorf("postgres: post journal: event %d already linked to journal %d: %w", eventID, event.JournalID.Int64, core.ErrConflict)
	}

	if _, err := q.LinkEventJournal(ctx, sqlcgen.LinkEventJournalParams{
		ID:        eventID,
		JournalID: int64ToInt8(&journalID),
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return wrapStoreError("postgres: post journal: link event journal", err)
		}
		current, getErr := q.GetEvent(ctx, eventID)
		if getErr != nil {
			return fmt.Errorf("postgres: post journal: recheck event %d: %w", eventID, getErr)
		}
		if !current.JournalID.Valid || current.JournalID.Int64 != journalID {
			return fmt.Errorf("postgres: post journal: event %d already linked to a different journal: %w", eventID, core.ErrConflict)
		}
	}

	if event.BookingID == 0 {
		return nil
	}

	booking, err := q.GetBooking(ctx, event.BookingID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: post journal: booking %d from event %d: %w", event.BookingID, eventID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: post journal: get booking %d: %w", event.BookingID, err)
	}
	if booking.JournalID.Valid && booking.JournalID.Int64 != journalID {
		return fmt.Errorf("postgres: post journal: booking %d already linked to journal %d: %w", event.BookingID, booking.JournalID.Int64, core.ErrConflict)
	}

	if _, err := q.LinkBookingJournal(ctx, sqlcgen.LinkBookingJournalParams{
		ID:        event.BookingID,
		JournalID: int64ToInt8(&journalID),
	}); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return wrapStoreError("postgres: post journal: link booking journal", err)
		}
		current, getErr := q.GetBooking(ctx, event.BookingID)
		if getErr != nil {
			return fmt.Errorf("postgres: post journal: recheck booking %d: %w", event.BookingID, getErr)
		}
		if !current.JournalID.Valid || current.JournalID.Int64 != journalID {
			return fmt.Errorf("postgres: post journal: booking %d already linked to a different journal: %w", event.BookingID, core.ErrConflict)
		}
	}

	return nil
}

// GetBalance computes balance for a single (holder, currency, classification) dimension.
// Balance = checkpoint.balance + delta (entries since checkpoint).
// Delta computation respects normal_side of the classification.
//
// In pool mode, both queries run inside a REPEATABLE READ transaction to
// prevent phantom reads from concurrent journal writes between the two queries.
//
// In tx mode (store bound via withDB), no sub-transaction is started; the
// caller's transaction isolation level applies. If the caller requires
// snapshot consistency, it should begin its transaction with REPEATABLE READ
// before calling GetBalance.
func (s *LedgerStore) GetBalance(ctx context.Context, holder int64, currencyUID, classificationUID string) (decimal.Decimal, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.get_balance",
		attribute.Int64("account_holder", holder),
		attribute.String("currency_uid", currencyUID),
		attribute.String("classification_uid", classificationUID),
	)
	defer span.End()

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return decimal.Zero, err
	}
	cls, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return decimal.Zero, err
	}
	currencyID, classificationID := cur.ID, cls.ID

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly — no inner tx.
		bal, err := s.getBalanceWithQueries(ctx, s.q, holder, currencyID, classificationID)
		ledgerotel.RecordError(span, err)
		return bal, err
	}

	// Pool mode: wrap in REPEATABLE READ to prevent phantom reads between the
	// checkpoint query and the entry-sum query.
	tx, txErr := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if txErr != nil {
		ledgerotel.RecordError(span, txErr)
		return decimal.Zero, fmt.Errorf("postgres: get balance: begin tx: %w", txErr)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	bal, err := s.getBalanceWithQueries(ctx, qtx, holder, currencyID, classificationID)
	ledgerotel.RecordError(span, err)
	return bal, err
}

// getBalanceWithQueries is the shared inner implementation of GetBalance. It
// executes against whichever *sqlcgen.Queries is provided (pool-backed or
// tx-backed). The caller is responsible for transaction lifecycle.
func (s *LedgerStore) getBalanceWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, classificationID int64) (decimal.Decimal, error) {
	// Get checkpoint (may not exist yet)
	var checkpointBalance decimal.Decimal
	var sinceEntryID int64

	cp, err := q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return decimal.Zero, fmt.Errorf("postgres: get balance: checkpoint: %w", err)
	}
	if err == nil {
		checkpointBalance = mustNumericToDecimal(cp.Balance)
		sinceEntryID = cp.LastEntryID
	}

	// Get entry sums since checkpoint
	sums, err := q.SumEntriesSinceForClassification(ctx, sqlcgen.SumEntriesSinceForClassificationParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
		SinceEntryID:     sinceEntryID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: sum entries: %w", err)
	}

	// We need the normal_side to compute balance direction.
	var debitSum, creditSum decimal.Decimal
	for _, row := range sums {
		amount, err := anyToDecimal(row.Total)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: get balance: convert total: %w", err)
		}
		switch core.EntryType(row.EntryType) {
		case core.EntryTypeDebit:
			debitSum = debitSum.Add(amount)
		case core.EntryTypeCredit:
			creditSum = creditSum.Add(amount)
		}
	}

	// Get classification to determine normal_side
	cls, err := q.GetClassification(ctx, classificationID)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: get classification %d: %w", classificationID, err)
	}
	normalSide := core.NormalSide(cls.NormalSide)

	// core.Delta is the sole authority for this computation (I-43). This
	// used to default to debit-normal for any unrecognized normal_side;
	// core.Delta refuses instead.
	delta, err := core.Delta(normalSide, debitSum, creditSum)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: get balance: classification %d: %w", classificationID, err)
	}

	return checkpointBalance.Add(delta), nil
}

// GetBalanceBreakdown aggregates the holder's classification balances by
// core.BalanceRole and layers reservation holds on top:
//
//	pending   = Σ balance(role=pending)
//	locked    = Σ balance(role=locked) + held (reservations)
//	available = Σ balance(role=available) − held
//	total     = available + locked + pending
//
// In pool mode the whole read runs inside one REPEATABLE READ transaction so
// the role sums and the holds figure describe the same point in time. In tx
// mode the caller's transaction (and isolation level) applies.
func (s *LedgerStore) GetBalanceBreakdown(ctx context.Context, holder int64, currencyUID string) (*core.BalanceBreakdown, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.ledger.get_balance_breakdown",
		attribute.Int64("account_holder", holder),
		attribute.String("currency_uid", currencyUID),
	)
	defer span.End()

	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if s.pool == nil {
		b, err := s.getBalanceBreakdownWithQueries(ctx, s.q, holder, cur.ID, currencyUID)
		ledgerotel.RecordError(span, err)
		return b, err
	}

	tx, txErr := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if txErr != nil {
		ledgerotel.RecordError(span, txErr)
		return nil, fmt.Errorf("postgres: get balance breakdown: begin tx: %w", txErr)
	}
	defer tx.Rollback(ctx)

	b, err := s.getBalanceBreakdownWithQueries(ctx, s.q.WithTx(tx), holder, cur.ID, currencyUID)
	ledgerotel.RecordError(span, err)
	return b, err
}

func (s *LedgerStore) getBalanceBreakdownWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID int64, currencyUID string) (*core.BalanceBreakdown, error) {
	roleSums, err := s.sumBalancesByRoleWithQueries(ctx, q, holder, currencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: %w", err)
	}

	heldRaw, err := q.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: sum reservations: %w", err)
	}
	held, err := anyToDecimal(heldRaw)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balance breakdown: convert held: %w", err)
	}

	available := roleSums[core.BalanceRoleAvailable].Sub(held)
	pending := roleSums[core.BalanceRolePending]
	locked := roleSums[core.BalanceRoleLocked].Add(held)

	return &core.BalanceBreakdown{
		AccountHolder: holder,
		CurrencyUID:   currencyUID,
		Available:     available,
		Pending:       pending,
		Locked:        locked,
		Total:         available.Add(locked).Add(pending),
	}, nil
}

// sumBalancesByRoleWithQueries sums checkpoint+delta balances of every
// classification the holder has entries in, bucketed by the classification's
// balance_role. Role-less (”) classifications are skipped. Roles are read
// fresh from the config table (not the dims cache) because SetBalanceRole can
// retag a classification after creation — the dims cache only holds immutable
// fields.
func (s *LedgerStore) sumBalancesByRoleWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID int64) (map[core.BalanceRole]decimal.Decimal, error) {
	rows, err := q.ListComputedBalancesForHolders(ctx, sqlcgen.ListComputedBalancesForHoldersParams{
		CurrencyID: currencyID,
		HolderIds:  []int64{holder},
	})
	if err != nil {
		return nil, fmt.Errorf("compute account balances: %w", err)
	}

	sums := map[core.BalanceRole]decimal.Decimal{
		core.BalanceRoleAvailable: decimal.Zero,
		core.BalanceRolePending:   decimal.Zero,
		core.BalanceRoleLocked:    decimal.Zero,
	}
	for _, row := range rows {
		role := core.BalanceRole(row.BalanceRole)
		if role == core.BalanceRoleNone {
			continue
		}
		bal, err := numericToDecimal(row.Balance)
		if err != nil {
			return nil, fmt.Errorf("balance for classification %d: %w", row.ClassificationID, err)
		}
		sums[role] = sums[role].Add(bal)
	}
	return sums, nil
}

// GetBalances returns balances across all classifications for a (holder, currency).
func (s *LedgerStore) GetBalances(ctx context.Context, holder int64, currencyUID string) ([]core.Balance, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	result, err := s.computedBalances(ctx, []int64{holder}, cur.ID, currencyUID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get balances: %w", err)
	}
	return result[holder], nil
}

// BatchGetBalances returns balances for multiple holders.
func (s *LedgerStore) BatchGetBalances(ctx context.Context, holderIDs []int64, currencyUID string) (map[int64][]core.Balance, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return nil, err
	}
	result, err := s.computedBalances(ctx, holderIDs, cur.ID, currencyUID)
	if err != nil {
		return nil, fmt.Errorf("postgres: batch get balances: %w", err)
	}
	return result, nil
}

func (s *LedgerStore) computedBalances(ctx context.Context, holderIDs []int64, currencyID int64, currencyUID string) (map[int64][]core.Balance, error) {
	result := make(map[int64][]core.Balance, len(holderIDs))
	for _, holder := range holderIDs {
		result[holder] = []core.Balance{}
	}
	if len(holderIDs) == 0 {
		return result, nil
	}

	q := s.q
	if s.pool != nil {
		tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, fmt.Errorf("begin repeatable-read transaction: %w", err)
		}
		defer tx.Rollback(ctx)
		q = s.q.WithTx(tx)
	}

	rows, err := q.ListComputedBalancesForHolders(ctx, sqlcgen.ListComputedBalancesForHoldersParams{
		CurrencyID: currencyID,
		HolderIds:  holderIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("query computed balances: %w", err)
	}
	for _, row := range rows {
		balance, err := numericToDecimal(row.Balance)
		if err != nil {
			return nil, fmt.Errorf("classification %d balance: %w", row.ClassificationID, err)
		}
		result[row.AccountHolder] = append(result[row.AccountHolder], core.Balance{
			AccountHolder:     row.AccountHolder,
			CurrencyUID:       currencyUID,
			ClassificationUID: pgToUID(row.ClassificationUid),
			Balance:           balance,
		})
	}
	return result, nil
}
