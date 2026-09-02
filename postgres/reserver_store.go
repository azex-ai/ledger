package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.Reserver = (*ReserverStore)(nil)

// ReserverStore implements core.Reserver using PostgreSQL with advisory locks.
//
// In pool mode (constructed via NewReserverStore), each write operation starts
// its own transaction. In tx mode (bound via withDB), write operations
// participate in the caller's transaction — commit/rollback is the caller's
// responsibility.
type ReserverStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool   *pgxpool.Pool
	db     DBTX
	q      *sqlcgen.Queries
	ledger *LedgerStore
	dims   *dimCache
	// verifiedBalance backs ReserveInput.RequireVerifiedBalance (contracts
	// §W2-2). Never nil -- ledger.go always wires a VerifiedBalanceStore,
	// even when no core.AuthVerifier was configured (in which case any
	// dimension with contributing journals simply comes back
	// ErrUnauthorizedJournal, per VerifiedBalanceStore's own doc comment) --
	// so this field being unusable is exactly the caller-opted-in-without-
	// signing-enabled case, not a nil-pointer bug.
	verifiedBalance *VerifiedBalanceStore
}

// NewReserverStore creates a new ReserverStore backed by a connection pool.
func NewReserverStore(pool *pgxpool.Pool, ledger *LedgerStore, verifiedBalance *VerifiedBalanceStore) *ReserverStore {
	return &ReserverStore{
		pool:            pool,
		db:              pool,
		q:               sqlcgen.New(pool),
		ledger:          ledger,
		dims:            dimCacheFor(pool),
		verifiedBalance: verifiedBalance,
	}
}

// WithDB returns a clone of the ReserverStore bound to an existing
// transaction. ledger must be a LedgerStore already bound to the same
// transaction so that balance checks and advisory locks share the same
// connection. verifiedBalance is intentionally NOT re-bound to db: it is
// only ever consulted from Reserve's top level, strictly before any
// transaction is opened (mirroring Authorize's own "outside the
// transaction" placement rule) -- see Reserve's doc comment.
func (s *ReserverStore) WithDB(db DBTX, ledger *LedgerStore) *ReserverStore {
	return &ReserverStore{
		dims:            s.dims,
		pool:            nil, // tx mode
		db:              db,
		q:               sqlcgen.New(db),
		ledger:          ledger,
		verifiedBalance: s.verifiedBalance,
	}
}

// Reserve creates an amount reservation with advisory lock serialization.
// Idempotent: same key + same payload returns the existing reservation;
// divergent payload returns ErrConflict.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the reservation is written into the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.reserve",
		attribute.Int64("account_holder", input.AccountHolder),
		attribute.String("currency_uid", input.CurrencyUID),
		attribute.String("idempotency_key", input.IdempotencyKey),
		attribute.String("amount", input.Amount.String()),
	)
	defer span.End()

	if err := input.Validate(); err != nil {
		err := fmt.Errorf("postgres: reserve: %w", err)
		ledgerotel.RecordError(span, err)
		return nil, err
	}
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, input.CurrencyUID)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}
	if err := checkAmountPrecision(input.Amount, cur); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	// Opt-in authorization gate (contracts §W2-2/§W2-3): only runs when the
	// caller set RequireVerifiedBalance. Deliberately BEFORE any transaction
	// is opened, on the same placement rule Authorize follows -- an
	// AuthVerifier implementation is permitted to be a remote call (see
	// core.AuthVerifier's doc comment), so financial.md's "no external
	// calls inside a transaction" applies here exactly as it does to
	// Attestor.Sign.
	//
	// s.pool == nil means this store is tx-bound (constructed via WithDB,
	// reachable from inside a caller's ledger.Service.RunInTx callback): a
	// transaction is already open, so running the gate here would be the
	// exact violation the comment above describes -- verifiedBalance is
	// deliberately NOT re-bound to the transaction in WithDB (see that
	// method's doc comment) precisely so this path has no connection to
	// silently call out on. Fail closed instead of doing so anyway, mirroring
	// LedgerStore.Authorize's identical guard (concurrency.md Major: this gate
	// previously copied Authorize's placement comment but not its guard).
	//
	// The gate does not only authorize; it also decides the amount. What it
	// computes is the entries-only recompute of every available-role
	// classification, and that figure -- not checkpoint + delta -- is what
	// caps the reservation below, together with an under-lock recompute of
	// the same sum (I-49).
	var verifiedAvailableBase *decimal.Decimal
	if input.RequireVerifiedBalance {
		if s.pool == nil {
			err := fmt.Errorf("postgres: reserve: called on a transaction-bound store with RequireVerifiedBalance=true; the verified-balance gate may call a remote AuthVerifier and financial.md forbids that inside an open transaction -- call Reserve with RequireVerifiedBalance set BEFORE opening a RunInTx, not from inside its callback: %w", core.ErrInvalidInput)
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		verified, err := s.requireVerifiedAvailableBalance(ctx, input.AccountHolder, input.CurrencyUID, cur.ID)
		if err != nil {
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		verifiedAvailableBase = &verified
	}

	// Check idempotency first (outside tx / on the current db handle).
	existing, err := s.q.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureReservationMatchesInput(ctx, s.q, existing, input, cur.ID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: check idempotency: %w", err)
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		res, err := s.reserveWithQueries(ctx, s.q, input, cur.ID, verifiedAvailableBase)
		ledgerotel.RecordError(span, err)
		return res, err
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	res, err := s.reserveWithQueries(ctx, qtx, input, cur.ID, verifiedAvailableBase)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: commit: %w", err)
	}

	return res, nil
}

// requireVerifiedAvailableBalance is Reserve's ReserveInput.RequireVerifiedBalance
// gate (contracts §W2-2, docs/INVARIANTS.md I-32 + I-49). It does two things,
// and the second one is the reason it returns a number:
//
//   - Authorization: every balance_role='available' classification the holder
//     has touched in this currency must individually pass
//     VerifiedBalanceReader's check. A single unauthorized classification
//     refuses the whole reservation -- fail-closed, never "sum the ones that
//     passed" (I-32's Why section: excluding an unauthorized reversal would
//     report MORE money, not less).
//
//   - Amount: the per-classification figures VerifiedBalance returns are
//     entries-only recomputes (CheckpointIntegrityStore.RecomputeBalance),
//     and their sum is what the caller gets back. Reserve uses it in place of
//     the checkpoint+delta roleSums, so a tampered balance_checkpoints row
//     cannot inflate what a gated reservation may lock. This used to be
//     discarded; the design's §0 decision table ("checkpoint is an untrusted
//     cache; the withdrawal path recomputes in full and does not read it")
//     was therefore not implemented on the one primitive that pays out.
//
// The list of classifications comes from ListComputedBalancesForHolders, whose
// `populated` CTE enumerates DISTINCT journal_entries rows -- entries, not
// checkpoints. A checkpoint row for a dimension with no entries cannot
// conjure a classification into this loop, and only row.BalanceRole (a config
// column, guarded by the config-table triggers) is read off it. The `balance`
// column it also returns is deliberately unused here.
//
// Role is re-derived per call (not read from the dims cache) for the same
// reason sumBalancesByRoleWithQueries does it: SetBalanceRole can retag a
// classification after creation, and the dims cache only holds immutable
// fields.
//
// Placement: this runs OUTSIDE the transaction, because an AuthVerifier may
// be a remote call. The figure it returns is therefore authorized but not
// current -- a journal committed between here and the advisory lock can leave
// it stale-HIGH (a spend lowers the true balance), which on its own would
// re-open the very over-sell race I-4's lock exists to close. It is not used
// on its own: reserveWithQueries re-derives the same sum from entries under
// the lock, in pure SQL, and takes the minimum of the two. See that
// function's comment for why each figure covers the other's blind spot.
// Verifying under the lock instead is not an option -- it would put the
// verifier call inside the transaction, which financial.md forbids.
func (s *ReserverStore) requireVerifiedAvailableBalance(ctx context.Context, holder int64, currencyUID string, currencyID int64) (decimal.Decimal, error) {
	rows, err := s.q.ListComputedBalancesForHolders(ctx, sqlcgen.ListComputedBalancesForHoldersParams{
		CurrencyID: currencyID,
		HolderIds:  []int64{holder},
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: reserve: verified balance: list classifications: %w", err)
	}

	total := decimal.Zero
	for _, row := range rows {
		if core.BalanceRole(row.BalanceRole) != core.BalanceRoleAvailable {
			continue
		}
		classUID := pgToUID(row.ClassificationUid)
		verified, err := s.verifiedBalance.VerifiedBalance(ctx, holder, currencyUID, classUID)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: reserve: verified balance check failed for classification %s: %w", classUID, err)
		}
		total = total.Add(verified)
	}
	return total, nil
}

// sumAvailableFromEntriesWithQueries is the under-lock, transaction-local half
// of I-49's available base: the same per-classification entries-only sum
// CheckpointIntegrityStore.RecomputeBalance runs
// (RecomputeCheckpointFromEntries — balance_checkpoints appears nowhere in its
// FROM/JOIN), totalled over every balance_role='available' classification the
// holder has entries in.
//
// Pure SQL on the caller's transaction, deliberately: this runs while the
// (holder, currency) advisory lock is held, so it must not make an external
// call. It is therefore current but unauthenticated — an unsigned forgery
// counts here — which is exactly why reserveWithQueries takes the minimum of
// this and the gate's authorized figure rather than either alone.
//
// One query per available classification rather than a single aggregate: a
// holder has a handful of available-role classifications (usually one), the
// generated query already exists and is the same trusted basis I-23 names,
// and reusing it keeps "entries-only recompute" implemented in exactly one
// place. The `balance` column ListComputedBalancesForHolders also returns is
// checkpoint + delta and is ignored here; only the entries-derived
// enumeration and the config-side balance_role are read from it.
func (s *ReserverStore) sumAvailableFromEntriesWithQueries(ctx context.Context, qtx *sqlcgen.Queries, holder, currencyID int64) (decimal.Decimal, error) {
	rows, err := qtx.ListComputedBalancesForHolders(ctx, sqlcgen.ListComputedBalancesForHoldersParams{
		CurrencyID: currencyID,
		HolderIds:  []int64{holder},
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: reserve: recompute available from entries: list classifications: %w", err)
	}

	total := decimal.Zero
	for _, row := range rows {
		if core.BalanceRole(row.BalanceRole) != core.BalanceRoleAvailable {
			continue
		}
		recomputed, err := qtx.RecomputeCheckpointFromEntries(ctx, sqlcgen.RecomputeCheckpointFromEntriesParams{
			AccountHolder:    holder,
			CurrencyID:       currencyID,
			ClassificationID: row.ClassificationID,
		})
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: reserve: recompute available from entries: classification %d: %w", row.ClassificationID, err)
		}
		balance, err := numericToDecimal(recomputed.Balance)
		if err != nil {
			return decimal.Zero, fmt.Errorf("postgres: reserve: recompute available from entries: convert classification %d: %w", row.ClassificationID, err)
		}
		total = total.Add(balance)
	}
	return total, nil
}

// reserveWithQueries writes the reservation. verifiedAvailableBase is non-nil
// exactly when ReserveInput.RequireVerifiedBalance was set: it carries the
// entries-only available total requireVerifiedAvailableBalance computed
// outside the transaction, and is combined with an under-lock recompute to
// form the available base below. Nil restores the ungated behavior byte for
// byte.
func (s *ReserverStore) reserveWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.ReserveInput, currencyID int64, verifiedAvailableBase *decimal.Decimal) (*core.Reservation, error) {
	if err := acquireIdempotencyLock(ctx, qtx, input.IdempotencyKey); err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}

	existing, err := qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return s.ensureReservationMatchesInput(ctx, qtx, existing, input, currencyID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency in tx: %w", err)
	}

	// Invariant (matches LedgerStore.PostJournal): all balance-mutating tx must
	// take pg_advisory_xact_lock(holder, currency_id) for every affected pair,
	// in sorted order. Reserve only ever touches a single pair, but we still
	// route through the same helper (acquireBalanceLocks -> AcquireBalanceLock,
	// two-key advisory lock form, namespace 1 -- see that query's doc comment
	// in journals.sql) so the lock space stays genuinely consistent, not just
	// nominally so, across reserve and post-journal.
	if err := acquireBalanceLocks(ctx, qtx, []balancePair{{
		holder:     input.AccountHolder,
		currencyID: currencyID,
	}}); err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}

	// No second idempotency recheck needed: the advisory lock from
	// acquireIdempotencyLock above already serializes all same-key transactions,
	// so nothing could have inserted a matching row between then and now.

	// Account policy enforcement (I-17): Reserve is unconditionally a
	// consumption entry point (it locks funds toward future spend), so
	// frozen/closed both reject it outright — no direction/netting question
	// applies here the way it does for PostJournal entries. classificationID
	// is 0 because a reservation isn't tied to any classification; only the
	// (holder,currency,0) and (holder,0,0) policy tiers can ever match.
	// Evaluated inside the same advisory lock as the balance check below, so
	// it is TOCTOU-safe against a concurrent SetPolicy on the same pair.
	policy, err := getEffectiveAccountPolicy(ctx, qtx, input.AccountHolder, currencyID, 0)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}
	if policy != nil {
		switch core.AccountPolicyStatus(policy.Status) {
		case core.AccountPolicyStatusClosed:
			return nil, fmt.Errorf("postgres: reserve: account %d currency %s is closed (policy %d): %w", input.AccountHolder, input.CurrencyUID, policy.ID, core.ErrAccountClosed)
		case core.AccountPolicyStatusFrozen:
			return nil, fmt.Errorf("postgres: reserve: account %d currency %s is frozen (policy %d): %w", input.AccountHolder, input.CurrencyUID, policy.ID, core.ErrAccountFrozen)
		}
	}

	// Check sufficient balance before reserving.
	// The advisory lock above serializes concurrent reserves for the same (holder, currency),
	// so this read is safe against TOCTOU races.
	//
	// The availability base is the sum of balance_role='available'
	// classifications ONLY — pending deposits (role=pending), journal-locked
	// funds (role=locked), and role-less classifications (fee_expense etc.)
	// are not reservable. This is the same figure GetBalanceBreakdown reports
	// as Available (before subtracting holds).
	//
	// Under the RequireVerifiedBalance gate the base is instead min(V, E) of
	// two entries-only figures (I-49), neither of which reads
	// balance_checkpoints — the one balance-bearing table an attacker holding
	// the app's DB credential can UPDATE (it has no append-only trigger; the
	// rollup worker must be able to write it):
	//
	//   V = verifiedAvailableBase, computed by the gate BEFORE the
	//       transaction opened, because it needs a possibly-remote
	//       AuthVerifier and financial.md forbids external calls inside a
	//       transaction. Authorized, but as of a moment now past.
	//   E = sumAvailableFromEntriesWithQueries, recomputed HERE, under the
	//       (holder, currency) advisory lock, in pure SQL with no external
	//       call. Current, but it cannot tell a genuine journal from a forged
	//       one.
	//
	// Each covers the other's blind spot, and taking the minimum is what
	// makes the pair safe in both directions:
	//
	//   - A journal committed in the gate's window that SPENDS money leaves
	//     V stale-high. E sees it, E < V, the reservation is sized off E. This
	//     is I-4's over-sell hazard, and it is why V alone is not enough: the
	//     pre-fix ungated read was inside this lock, so moving the base
	//     outside it without this recheck would have traded a tampering hole
	//     for a TOCTOU hole.
	//   - A forged, unsigned journal committed in that same window CREDITS
	//     money. E sees it too — E has no authorization check — but E > V, so
	//     V wins and the forgery buys nothing. This is why E alone is not
	//     enough either.
	//
	// Holds are subtracted from either base identically: reservations are not
	// part of what a checkpoint or an unsigned journal can misstate.
	var availableBase decimal.Decimal
	if verifiedAvailableBase != nil {
		entriesBase, err := s.sumAvailableFromEntriesWithQueries(ctx, qtx, input.AccountHolder, currencyID)
		if err != nil {
			return nil, err
		}
		availableBase = decimal.Min(*verifiedAvailableBase, entriesBase)
	} else {
		roleSums, err := s.ledger.sumBalancesByRoleWithQueries(ctx, qtx, input.AccountHolder, currencyID)
		if err != nil {
			return nil, fmt.Errorf("postgres: reserve: sum balances by role: %w", err)
		}
		availableBase = roleSums[core.BalanceRoleAvailable]
	}

	activeReserved, err := qtx.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: input.AccountHolder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: sum active reservations: %w", err)
	}
	activeReservedDecimal, err := anyToDecimal(activeReserved)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: convert active reservations: %w", err)
	}

	available := availableBase.Sub(activeReservedDecimal)
	if available.LessThan(input.Amount) {
		return nil, fmt.Errorf("postgres: reserve: available %s < requested %s: %w", available.String(), input.Amount.String(), core.ErrInsufficientBalance)
	}

	expiresAt := time.Now().Add(resolveReservationExpiresIn(input.ExpiresIn))

	row, err := qtx.InsertReservation(ctx, sqlcgen.InsertReservationParams{
		AccountHolder:  input.AccountHolder,
		CurrencyID:     currencyID,
		ReservedAmount: decimalToNumeric(input.Amount),
		IdempotencyKey: input.IdempotencyKey,
		ExpiresAt:      expiresAt,
		Uid:            newUID(),
	})
	if err != nil {
		existing, lookupErr := qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return s.ensureReservationMatchesInput(ctx, qtx, existing, input, currencyID)
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reserve: insert: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		}
		return nil, wrapStoreError("postgres: reserve: insert", err)
	}

	return reservationFromRow(ctx, s.dims, qtx, row)
}

// reservationDefaultExpiresIn is applied when ReserveInput.ExpiresIn is zero.
const reservationDefaultExpiresIn = 15 * time.Minute

// resolveReservationExpiresIn returns the duration that will be added to
// time.Now() when storing ExpiresAt. Both the insert path and the idempotency
// match path use it so retries with the same input compare equal.
func resolveReservationExpiresIn(d time.Duration) time.Duration {
	if d == 0 {
		return reservationDefaultExpiresIn
	}
	return d
}

// Reservation operation names recorded in reservation_operation_receipts
// (migration 005). These identify WHICH terminal transition applied a given
// idempotency key, so reusing a key for a different operation on the same
// reservation is a payload mismatch (ErrConflict), not a silent success.
const (
	reservationOpSettle             = "settle"
	reservationOpRelease            = "release"
	reservationOpFinalizeSettlement = "finalize_settlement"
)

// ensureReservationOperationReceiptMatches is Settle/Release/FinalizeSettlement's
// shared idempotency check (I-3), mirroring ensureReservationMatchesInput and
// SettlePartial's own leg-comparison pattern: a replay with the same
// reservation, operation and amount short-circuits to success (nil); any
// divergence -- different reservation, different operation, or different
// amount -- is core.ErrConflict.
func ensureReservationOperationReceiptMatches(receipt sqlcgen.ReservationOperationReceipt, reservationID int64, operation string, amount decimal.Decimal, idempotencyKey string) error {
	if receipt.ReservationID != reservationID {
		return fmt.Errorf("postgres: %s: idempotency key %q already used for a different reservation: %w", operation, idempotencyKey, core.ErrConflict)
	}
	if receipt.Operation != operation {
		return fmt.Errorf("postgres: %s: idempotency key %q already used for a different operation (%s): %w", operation, idempotencyKey, receipt.Operation, core.ErrConflict)
	}
	receiptAmount, err := numericToDecimal(receipt.Amount)
	if err != nil {
		return fmt.Errorf("postgres: %s: convert receipt amount: %w", operation, err)
	}
	if !receiptAmount.Equal(amount) {
		return fmt.Errorf("postgres: %s: idempotency key %q payload mismatch (recorded %s, got %s): %w", operation, idempotencyKey, receiptAmount, amount, core.ErrConflict)
	}
	return nil
}

// recordReservationOperationReceipt inserts the durable idempotency record
// for a just-applied Settle/Release/FinalizeSettlement call. A 23505 on the
// unique idempotency_key index (caught via ON CONFLICT DO NOTHING returning
// no row, surfaced here as pgx.ErrNoRows) means a concurrent call on a
// DIFFERENT reservation raced this one to the same key after the caller's
// own pre-check -- the row lock on THIS reservation already serializes
// same-reservation racers, so this can only be a cross-reservation collision
// and is reported as ErrConflict, matching InsertReservationSettlementLeg's
// existing race-handling shape.
func (s *ReserverStore) recordReservationOperationReceipt(ctx context.Context, qtx *sqlcgen.Queries, reservationID int64, operation string, amount decimal.Decimal, idempotencyKey string) error {
	if _, err := qtx.InsertReservationOperationReceipt(ctx, sqlcgen.InsertReservationOperationReceiptParams{
		ReservationID:  reservationID,
		Operation:      operation,
		IdempotencyKey: idempotencyKey,
		Amount:         decimalToNumeric(amount),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: %s: idempotency key %q raced a concurrent application: %w", operation, idempotencyKey, core.ErrConflict)
		}
		return wrapStoreError(fmt.Sprintf("postgres: %s: record receipt", operation), err)
	}
	return nil
}

// Settle marks a reservation as settled with the actual amount.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Settle(ctx context.Context, input core.SettleInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.settle",
		attribute.String("reservation_uid", input.ReservationUID),
		attribute.String("actual_amount", input.Amount.String()),
		attribute.String("idempotency_key", input.IdempotencyKey),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		err := s.settleWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.settleWithQueries(ctx, s.q.WithTx(tx), input); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) settleWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.SettleInput) error {
	reservationUID, actualAmount := input.ReservationUID, input.Amount
	if !actualAmount.IsPositive() {
		return fmt.Errorf("postgres: settle: actual amount must be positive, got %s: %w", actualAmount, core.ErrInvalidInput)
	}

	pgUID, err := uidToPG(reservationUID)
	if err != nil {
		return err
	}
	res, err := qtx.GetReservationForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: settle: reservation %q: %w", reservationUID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: settle: get reservation: %w", err)
	}
	reservationID := res.ID

	// Idempotent replay short-circuit (I-3), checked under the row lock and
	// BEFORE the status gate: a retried call whose first application already
	// settled the reservation must return success, not ErrInvalidTransition.
	if receipt, err := qtx.GetReservationOperationReceiptByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
		return ensureReservationOperationReceiptMatches(receipt, reservationID, reservationOpSettle, actualAmount, input.IdempotencyKey)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: settle: check idempotency: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if status == core.ReservationStatusSettling {
		// The reservation FSM technically allows settling -> settled (that is
		// exactly the transition FinalizeSettlement performs), but a one-shot
		// Settle here would overwrite settled_amount with actualAmount instead
		// of respecting what SettlePartial already accumulated. Reject
		// explicitly rather than silently discarding prior partial settlements.
		return fmt.Errorf("postgres: settle: reservation %q is partially settled (status settling); use FinalizeSettlement, not Settle: %w", reservationUID, core.ErrInvalidTransition)
	}
	if !status.CanTransitionTo(core.ReservationStatusSettled) {
		return fmt.Errorf("postgres: settle: from %q to settled: %w", res.Status, core.ErrInvalidTransition)
	}

	// Business precision (I-16): the settled amount must respect the
	// reservation currency's exponent, same as Reserve's own amount.
	cur, err := s.dims.currencyByIDOrErr(ctx, qtx, res.CurrencyID)
	if err != nil {
		return fmt.Errorf("postgres: settle: %w", err)
	}
	if err := checkAmountPrecision(actualAmount, cur); err != nil {
		return fmt.Errorf("postgres: settle: %w", err)
	}

	// The reservations table enforces settled_amount <= reserved_amount via
	// chk_settled_lte_reserved, but check here too so callers get a clear
	// core.ErrInvalidInput without a round trip to the DB constraint.
	reservedAmount, err := numericToDecimal(res.ReservedAmount)
	if err != nil {
		return fmt.Errorf("postgres: settle: convert reserved amount: %w", err)
	}
	if actualAmount.GreaterThan(reservedAmount) {
		return fmt.Errorf("postgres: settle: actual amount %s exceeds reserved amount %s: %w", actualAmount, reservedAmount, core.ErrInvalidInput)
	}

	if err := qtx.UpdateReservationSettle(ctx, sqlcgen.UpdateReservationSettleParams{
		ID:            reservationID,
		SettledAmount: decimalToNumeric(actualAmount),
		JournalID:     pgtype.Int8{}, // no journal linked by the one-shot settle
	}); err != nil {
		return wrapStoreError("postgres: settle: update", err)
	}

	return s.recordReservationOperationReceipt(ctx, qtx, reservationID, reservationOpSettle, actualAmount, input.IdempotencyKey)
}

// SettlePartial settles part of a reservation, accumulating settled_amount.
// The first call transitions the reservation from active to settling.
//
// In pool mode a new transaction is started and committed here. In tx mode
// (bound via WithDB) the update is applied to the caller's transaction;
// commit/rollback is the caller's responsibility.
func (s *ReserverStore) SettlePartial(ctx context.Context, input core.SettlePartialInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	reservationUID, amount := input.ReservationUID, input.Amount
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.settle_partial",
		attribute.String("reservation_uid", reservationUID),
		attribute.String("amount", amount.String()),
	)
	defer span.End()

	if s.pool == nil {
		err := s.settlePartialWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle partial: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.settlePartialWithQueries(ctx, s.q.WithTx(tx), input); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle partial: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) settlePartialWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.SettlePartialInput) error {
	reservationUID, amount := input.ReservationUID, input.Amount
	if !amount.IsPositive() {
		return fmt.Errorf("postgres: settle partial: amount must be positive, got %s: %w", amount, core.ErrInvalidInput)
	}

	pgUID, err := uidToPG(reservationUID)
	if err != nil {
		return err
	}
	res, err := qtx.GetReservationForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: settle partial: reservation %q: %w", reservationUID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: settle partial: get reservation: %w", err)
	}
	reservationID := res.ID

	// Idempotent replay short-circuit (I-3), checked under the row lock and
	// BEFORE the status gate: a retried leg whose first application already
	// finalized the reservation must return success, not ErrInvalidTransition.
	if leg, err := qtx.GetSettlementLegByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
		if leg.ReservationID != reservationID {
			return fmt.Errorf("postgres: settle partial: idempotency key %q already used for a different reservation: %w", input.IdempotencyKey, core.ErrConflict)
		}
		legAmount, convErr := numericToDecimal(leg.Amount)
		if convErr != nil {
			return fmt.Errorf("postgres: settle partial: convert leg amount: %w", convErr)
		}
		if !legAmount.Equal(amount) {
			return fmt.Errorf("postgres: settle partial: idempotency key %q payload mismatch (recorded %s, got %s): %w", input.IdempotencyKey, legAmount, amount, core.ErrConflict)
		}
		return nil // already applied — do not accumulate again
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: settle partial: check idempotency: %w", err)
	}

	// Business precision (I-16): the increment must respect the reservation
	// currency's exponent, same as Reserve's own amount.
	cur, err := s.dims.currencyByIDOrErr(ctx, qtx, res.CurrencyID)
	if err != nil {
		return fmt.Errorf("postgres: settle partial: %w", err)
	}
	if err := checkAmountPrecision(amount, cur); err != nil {
		return fmt.Errorf("postgres: settle partial: %w", err)
	}

	// active -> settling (first call) or settling -> settling (accumulating
	// further) are both valid; every other status is not — in particular a
	// reservation that is already settled or released cannot un-terminal
	// itself via SettlePartial.
	status := core.ReservationStatus(res.Status)
	if status != core.ReservationStatusActive && status != core.ReservationStatusSettling {
		return fmt.Errorf("postgres: settle partial: from %q: %w", res.Status, core.ErrInvalidTransition)
	}

	reservedAmount, err := numericToDecimal(res.ReservedAmount)
	if err != nil {
		return fmt.Errorf("postgres: settle partial: convert reserved amount: %w", err)
	}
	settledSoFar, err := numericToDecimal(res.SettledAmount)
	if err != nil {
		return fmt.Errorf("postgres: settle partial: convert settled amount: %w", err)
	}

	// chk_settled_lte_reserved backstops this at the DB level, but check here
	// too so callers get a clear core.ErrInvalidInput without a round trip to
	// the DB constraint.
	newSettled := settledSoFar.Add(amount)
	if newSettled.GreaterThan(reservedAmount) {
		return fmt.Errorf("postgres: settle partial: cumulative settled %s exceeds reserved %s: %w", newSettled, reservedAmount, core.ErrInvalidInput)
	}

	// Record the leg and apply the increment in the same transaction: the leg
	// row is the durable proof this key was applied exactly once. The unique
	// index on idempotency_key backstops the check above (row lock already
	// serializes same-reservation racers; the index covers cross-reservation
	// key reuse).
	if _, err := qtx.InsertReservationSettlementLeg(ctx, sqlcgen.InsertReservationSettlementLegParams{
		ReservationID:  reservationID,
		IdempotencyKey: input.IdempotencyKey,
		Amount:         decimalToNumeric(amount),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// ON CONFLICT DO NOTHING fired: the key landed concurrently from
			// another transaction after our check. Treat as conflict — the
			// caller retries and hits the idempotent path above.
			return fmt.Errorf("postgres: settle partial: idempotency key %q raced a concurrent application: %w", input.IdempotencyKey, core.ErrConflict)
		}
		return wrapStoreError("postgres: settle partial: record leg", err)
	}

	if err := qtx.SettleReservationPartial(ctx, sqlcgen.SettleReservationPartialParams{
		ID:            reservationID,
		SettledAmount: decimalToNumeric(amount),
	}); err != nil {
		return wrapStoreError("postgres: settle partial: update", err)
	}

	return nil
}

// FinalizeSettlement completes a reservation that has been partially settled
// via SettlePartial, transitioning it from settling to settled. Any status
// other than settling — including active, which never received a
// SettlePartial call — is rejected with ErrInvalidTransition; Settle is the
// one-shot equivalent for a reservation that was never partially settled.
//
// In pool mode a new transaction is started and committed here. In tx mode
// (bound via WithDB) the update is applied to the caller's transaction;
// commit/rollback is the caller's responsibility.
func (s *ReserverStore) FinalizeSettlement(ctx context.Context, input core.FinalizeSettlementInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.finalize_settlement",
		attribute.String("reservation_uid", input.ReservationUID),
		attribute.String("idempotency_key", input.IdempotencyKey),
	)
	defer span.End()

	if s.pool == nil {
		err := s.finalizeSettlementWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: finalize settlement: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.finalizeSettlementWithQueries(ctx, s.q.WithTx(tx), input); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: finalize settlement: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) finalizeSettlementWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.FinalizeSettlementInput) error {
	reservationUID := input.ReservationUID
	pgUID, err := uidToPG(reservationUID)
	if err != nil {
		return err
	}
	res, err := qtx.GetReservationForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: finalize settlement: reservation %q: %w", reservationUID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: finalize settlement: get reservation: %w", err)
	}
	reservationID := res.ID

	// Idempotent replay short-circuit (I-3), checked under the row lock and
	// BEFORE the status gate: a retried call whose first application already
	// finalized the reservation must return success, not ErrInvalidTransition.
	if receipt, err := qtx.GetReservationOperationReceiptByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
		return ensureReservationOperationReceiptMatches(receipt, reservationID, reservationOpFinalizeSettlement, decimal.Zero, input.IdempotencyKey)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: finalize settlement: check idempotency: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if status != core.ReservationStatusSettling {
		return fmt.Errorf("postgres: finalize settlement: reservation %q has status %q, not settling (use Settle for a one-shot settlement that never called SettlePartial): %w", reservationUID, res.Status, core.ErrInvalidTransition)
	}

	if err := qtx.FinalizeReservationSettlement(ctx, reservationID); err != nil {
		return wrapStoreError("postgres: finalize settlement: update", err)
	}

	return s.recordReservationOperationReceipt(ctx, qtx, reservationID, reservationOpFinalizeSettlement, decimal.Zero, input.IdempotencyKey)
}

// HeldAmount returns the holder's outstanding holds in the given currency:
// full reserved_amount for active reservations plus the unsettled remainder
// (reserved − settled) of settling ones. This is the same figure Reserve
// subtracts from balance when checking availability, exposed so consumers can
// compute available = balance − held without reaching into the reservations
// table.
func (s *ReserverStore) HeldAmount(ctx context.Context, holder int64, currencyUID string) (decimal.Decimal, error) {
	cur, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
	if err != nil {
		return decimal.Zero, err
	}
	total, err := s.q.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: held amount: %w", err)
	}
	held, err := anyToDecimal(total)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: held amount: convert: %w", err)
	}
	return held, nil
}

// Release cancels an active reservation.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Release(ctx context.Context, input core.ReleaseInput) error {
	if err := input.Validate(); err != nil {
		return err
	}
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.release",
		attribute.String("reservation_uid", input.ReservationUID),
		attribute.String("idempotency_key", input.IdempotencyKey),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		err := s.releaseWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: release: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.releaseWithQueries(ctx, s.q.WithTx(tx), input); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: release: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) releaseWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.ReleaseInput) error {
	reservationUID := input.ReservationUID
	pgUID, err := uidToPG(reservationUID)
	if err != nil {
		return err
	}
	res, err := qtx.GetReservationForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: release: reservation %q: %w", reservationUID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: release: get reservation: %w", err)
	}
	reservationID := res.ID

	// Idempotent replay short-circuit (I-3), checked under the row lock and
	// BEFORE the status gate: a retried call whose first application already
	// released the reservation must return success, not ErrInvalidTransition.
	if receipt, err := qtx.GetReservationOperationReceiptByIdempotencyKey(ctx, input.IdempotencyKey); err == nil {
		return ensureReservationOperationReceiptMatches(receipt, reservationID, reservationOpRelease, decimal.Zero, input.IdempotencyKey)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("postgres: release: check idempotency: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if !status.CanTransitionTo(core.ReservationStatusReleased) {
		return fmt.Errorf("postgres: release: from %q to released: %w", res.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateReservationStatus(ctx, sqlcgen.UpdateReservationStatusParams{
		ID:     reservationID,
		Status: string(core.ReservationStatusReleased),
	}); err != nil {
		return wrapStoreError("postgres: release: update", err)
	}

	return s.recordReservationOperationReceipt(ctx, qtx, reservationID, reservationOpRelease, decimal.Zero, input.IdempotencyKey)
}
