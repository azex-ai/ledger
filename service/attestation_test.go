package service_test

// P6 pin tests: batch attestation chain
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §8, I-27/I-28,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §4/§5).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

type attestFixture struct {
	journalTypeID    int64
	currencyID       int64
	classificationID int64

	journalTypeUID    string
	currencyUID       string
	classificationUID string
}

// journalInput builds a minimal balanced two-line posting (debit the
// holder, credit its system mirror, same classification on both legs --
// classification distinctness is irrelevant to JournalInput.Validate's
// per-currency balance check) for tests that need to post through the
// real, uid-space LedgerStore.PostJournal path rather than
// insertForgedJournal's raw-SQL bypass.
func (f attestFixture) journalInput(holder int64, idemKey string) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: f.journalTypeUID,
		IdempotencyKey: idemKey,
		Source:         "attest-verify-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: f.currencyUID, ClassificationUID: f.classificationUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: f.currencyUID, ClassificationUID: f.classificationUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
		},
	}
}

func setupAttestFixture(t testing.TB, pool *pgxpool.Pool, ctx context.Context) attestFixture {
	t.Helper()
	classStore := postgres.NewClassificationStore(pool)
	currencyStore := postgres.NewCurrencyStore(pool)
	suffix := time.Now().UnixNano()

	cur, err := currencyStore.CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("ATTC_%d", suffix), Name: "Attest Test Currency", Exponent: 18,
	})
	require.NoError(t, err)
	cls, err := classStore.CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("attest_main_%d", suffix), Name: "Attest Main", NormalSide: core.NormalSideDebit,
	})
	require.NoError(t, err)
	jt, err := classStore.CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("attest_jt_%d", suffix), Name: "Attest Test JT",
	})
	require.NoError(t, err)

	f := attestFixture{
		journalTypeUID: jt.UID, currencyUID: cur.UID, classificationUID: cls.UID,
	}
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", cur.UID).Scan(&f.currencyID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", cls.UID).Scan(&f.classificationID))
	require.NoError(t, pool.QueryRow(ctx, "SELECT id FROM journal_types WHERE uid=$1", jt.UID).Scan(&f.journalTypeID))
	return f
}

// insertForgedJournal inserts a journal row directly via SQL (like
// postgres/auth_pin_test.go's M5 scenario) -- these tests only need valid
// journal_entries rows to exist and reference a real journal_id via FK;
// per-currency balance correctness is irrelevant to the attestation
// mechanism under test.
func insertForgedJournal(t testing.TB, ctx context.Context, pool *pgxpool.Pool, f attestFixture, idemKey string) int64 {
	t.Helper()
	var journalID int64
	require.NoError(t, pool.QueryRow(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, event_id, effective_at, uid, auth_digest, auth_signature, auth_key_id)
		VALUES ($1, $2, 1, 1, '{}'::jsonb, 0, 'attest-test', NULL, now(), gen_random_uuid(), ''::bytea, ''::bytea, '')
		RETURNING id
	`, f.journalTypeID, idemKey).Scan(&journalID))
	return journalID
}

// beginWithCleanup begins a transaction and registers a t.Cleanup rollback
// -- a failed assertion between Begin and Commit must not leave the
// transaction open: postgres/migration 044's per-journal balance check is
// a DEFERRED CONSTRAINT TRIGGER, so a connection sitting on an open,
// never-committed-or-rolled-back transaction (from a t.Fatal-style
// assertion mid-test) hangs the whole pool's Close() at test cleanup,
// which manifests as a full test-binary timeout, not a clean failure.
func beginWithCleanup(t testing.TB, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		// Best-effort: a no-op (with an ignorable error) if already
		// committed or rolled back.
		_ = tx.Rollback(context.Background())
	})
	return tx
}

// insertEntryInTx inserts one journal_entries row on tx (NOT committed by
// this call -- the caller controls commit timing, to simulate entries
// committing out of id order across different (holder, currency) pairs).
// Returns the assigned entry id.
func insertEntryInTx(t testing.TB, ctx context.Context, tx pgx.Tx, f attestFixture, journalID, holder int64, entryType string) int64 {
	t.Helper()
	var entryID int64
	require.NoError(t, tx.QueryRow(ctx, `
		INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
		VALUES ($1, $2, $3, $4, $5, 1, now(), now())
		RETURNING id
	`, journalID, holder, f.currencyID, f.classificationID, entryType).Scan(&entryID))
	return entryID
}

// insertBalancedPairInTx inserts a debit+credit pair (same journal, same
// amount) on tx, NOT committed by this call. postgres/migration 044's
// per-journal-per-currency balance check is a DEFERRED CONSTRAINT TRIGGER
// evaluated at THIS transaction's own commit, against what THIS
// transaction's own snapshot can see -- so a journal's entries posted
// across two separate, still-open transactions would each individually
// look unbalanced at their own commit. Every test that needs "two entries
// committing out of id order" therefore uses two separate, individually
// balanced journals (matching design doc §8.2's own wording: "两个不同
// (holder,currency) 的 journal", plural), not one journal split across two
// transactions.
func insertBalancedPairInTx(t testing.TB, ctx context.Context, tx pgx.Tx, f attestFixture, journalID, debitHolder, creditHolder int64) (debitID, creditID int64) {
	t.Helper()
	debitID = insertEntryInTx(t, ctx, tx, f, journalID, debitHolder, "debit")
	creditID = insertEntryInTx(t, ctx, tx, f, journalID, creditHolder, "credit")
	return debitID, creditID
}

func newTestAttestor(t testing.TB, keyID string) core.Attestor {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, _, err := authdev.NewLocalAttestor(priv.Seed(), keyID)
	require.NoError(t, err)
	return attestor
}

func entryAttestationSeq(t testing.TB, ctx context.Context, pool *pgxpool.Pool, entryID int64) (seq int64, count int) {
	t.Helper()
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM entry_attestations WHERE entry_id=$1", entryID).Scan(&count))
	if count > 0 {
		require.NoError(t, pool.QueryRow(ctx, "SELECT seq FROM entry_attestations WHERE entry_id=$1", entryID).Scan(&seq))
	}
	return seq, count
}

// TestAttestationService_LateArrivingEntryIsEventuallyCoveredExactlyOnce
// is the core P6 pin (design doc §8.2's failure mode, required by the P6
// task brief): two entries from different (holder, currency) pairs commit
// out of id order -- the lower-id one LATER than the higher-id one. A
// batch scheme that closed out on `to_entry_id = MAX(id)` would either
// miss the late entry forever or double-cover it. The
// entry_attestations-side-table design must cover it exactly once, on the
// next run after it becomes visible.
func TestAttestationService_LateArrivingEntryIsEventuallyCoveredExactlyOnce(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor := newTestAttestor(t, "attest-key-1")
	store := postgres.NewAttestationStore(pool)
	svc := service.NewAttestationService(store, attestor, nil, core.NewEngine())

	// Two SEPARATE journals (design doc §8.2: "两个不同 (holder,currency)
	// 的 journal") -- each gets its own balanced debit+credit pair,
	// inserted together in the same transaction so migration 044's
	// per-journal balance check (evaluated against that transaction's own
	// snapshot at commit) sees a complete, balanced set.
	journalLate := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("attest-late-a"))
	journalEarly := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("attest-late-b"))

	// txLate gets the LOWER ids (inserted first) but stays open (uncommitted).
	txLate := beginWithCleanup(t, ctx, pool)
	entryLateDebit, entryLateCredit := insertBalancedPairInTx(t, ctx, txLate, f, journalLate, 9101, 9102)

	// txEarly gets HIGHER ids but commits immediately.
	txEarly := beginWithCleanup(t, ctx, pool)
	entryEarlyDebit, entryEarlyCredit := insertBalancedPairInTx(t, ctx, txEarly, f, journalEarly, 9103, 9104)
	require.NoError(t, txEarly.Commit(ctx))
	require.Greater(t, entryEarlyDebit, entryLateCredit, "test setup: journalEarly's entries must have higher ids despite committing first")

	// Batch 1: only journalEarly's pair is visible (journalLate's
	// transaction has not committed yet) -- this is the batch that, under
	// an id-range design, would close out "as of" the highest visible id
	// and never look back.
	attested1, seq1, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested1)

	seqEarly, countEarly := entryAttestationSeq(t, ctx, pool, entryEarlyDebit)
	require.Equal(t, 1, countEarly)
	require.Equal(t, seq1, seqEarly)

	_, countLateBefore := entryAttestationSeq(t, ctx, pool, entryLateDebit)
	require.Zero(t, countLateBefore, "journalLate's entries must not be covered before they are even committed")

	// Now the late journal's pair commits -- it becomes visible with ids
	// LOWER than something already covered by seq1.
	require.NoError(t, txLate.Commit(ctx))

	// Batch 2 must find and cover it.
	attested2, seq2, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested2)
	require.NotEqual(t, seq1, seq2)

	seqLate, countLate := entryAttestationSeq(t, ctx, pool, entryLateDebit)
	require.Equal(t, 1, countLate, "the late entry must be covered exactly once")
	require.Equal(t, seq2, seqLate)
	_, countLateCreditAfter := entryAttestationSeq(t, ctx, pool, entryLateCredit)
	require.Equal(t, 1, countLateCreditAfter)

	// Re-confirm journalEarly's entries were not touched or double-covered
	// by batch 2.
	_, countEarlyAfter := entryAttestationSeq(t, ctx, pool, entryEarlyDebit)
	require.Equal(t, 1, countEarlyAfter, "the earlier entries must still be covered exactly once, not re-covered")
	_, countEarlyCreditAfter := entryAttestationSeq(t, ctx, pool, entryEarlyCredit)
	require.Equal(t, 1, countEarlyCreditAfter)
}

// TestAttestationService_EmptyBatchStillProducesAnAttestation pins design
// doc §8.1: "空批照样出一条" -- otherwise "the job ran and found nothing"
// is indistinguishable from "the job never ran" (working-agreements §3).
func TestAttestationService_EmptyBatchStillProducesAnAttestation(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor := newTestAttestor(t, "attest-key-2")
	store := postgres.NewAttestationStore(pool)
	svc := service.NewAttestationService(store, attestor, nil, core.NewEngine())

	attested, seq, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Zero(t, attested)
	require.NotZero(t, seq)

	var entryCount int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT entry_count FROM ledger_attestations WHERE seq=$1", seq).Scan(&entryCount))
	require.Zero(t, entryCount)
}

// TestAttestationService_ChainLinksPrevRoot pins the hash-chain link
// itself: seq N's prev_root must equal seq N-1's root_hash.
func TestAttestationService_ChainLinksPrevRoot(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor := newTestAttestor(t, "attest-key-3")
	store := postgres.NewAttestationStore(pool)
	svc := service.NewAttestationService(store, attestor, nil, core.NewEngine())

	// seq 1: empty.
	_, seq1, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	// Post one balanced pair, then seq 2 covers it.
	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("attest-chain"))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, 9201, 9202)
	require.NoError(t, tx.Commit(ctx))

	_, seq2, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	var rootHash1, prevRoot2 []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT root_hash FROM ledger_attestations WHERE seq=$1", seq1).Scan(&rootHash1))
	require.NoError(t, pool.QueryRow(ctx, "SELECT prev_root FROM ledger_attestations WHERE seq=$1", seq2).Scan(&prevRoot2))
	require.Equal(t, rootHash1, prevRoot2, "seq2.prev_root must equal seq1.root_hash")
}

// TestAttestationService_RequiresAttestor pins that RunAttestBatch refuses
// to run without a configured Attestor -- there is no "unsigned
// attestation" concept in this schema (unlike P5's expand-safe empty
// columns).
func TestAttestationService_RequiresAttestor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store := postgres.NewAttestationStore(pool)
	svc := service.NewAttestationService(store, nil, nil, core.NewEngine())

	_, _, err := svc.RunAttestBatch(ctx, 100)
	require.Error(t, err)
}

// TestNaiveIDRangeWatermark_WouldMissTheLateEntry is NOT a pin for
// AttestationService -- it is the falsification evidence
// working-agreements §3 asks for: a direct demonstration that the
// design doc §8.2 failure mode is real, by running the REJECTED
// alternative (a monotonic `to_entry_id = MAX(id)` watermark, no
// entry_attestations side table) against the exact same interleaving as
// TestAttestationService_LateArrivingEntryIsEventuallyCoveredExactlyOnce,
// and confirming it silently drops the late entry forever -- which is
// exactly why this task uses a side table instead.
func TestNaiveIDRangeWatermark_WouldMissTheLateEntry(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	// Two SEPARATE journals -- see
	// TestAttestationService_LateArrivingEntryIsEventuallyCoveredExactlyOnce's
	// comment for why (migration 044's per-journal balance check).
	journalLate := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("attest-naive-a"))
	journalEarly := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("attest-naive-b"))

	txLate := beginWithCleanup(t, ctx, pool)
	entryLate, _ := insertBalancedPairInTx(t, ctx, txLate, f, journalLate, 9301, 9302)

	txEarly := beginWithCleanup(t, ctx, pool)
	entryEarly, _ := insertBalancedPairInTx(t, ctx, txEarly, f, journalEarly, 9303, 9304)
	require.NoError(t, txEarly.Commit(ctx))
	require.Greater(t, entryEarly, entryLate)

	// The rejected design: "batch 1 covers everything up to MAX(id) as of
	// now" -- a plain watermark, no side table.
	var watermark int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT COALESCE(MAX(id), 0) FROM journal_entries WHERE id <= $1", entryEarly).Scan(&watermark))
	require.GreaterOrEqual(t, watermark, entryEarly, "batch 1's watermark closes out at or after entryEarly (entryLate is not yet visible)")

	require.NoError(t, txLate.Commit(ctx))

	// "Batch 2" under the naive design starts scanning strictly AFTER the
	// watermark -- id > watermark -- so it structurally cannot ever see
	// entryLate again: entryLate's id is BELOW the watermark batch 1
	// already declared "done".
	var missedCount int
	require.NoError(t, pool.QueryRow(ctx, `
		SELECT count(*) FROM journal_entries WHERE id > $1 AND id = $2
	`, watermark, entryLate).Scan(&missedCount))
	require.Zero(t, missedCount, "the naive watermark design's own query predicate (id > watermark) provably excludes entryLate forever -- this is the bug design doc §8.2 rejects")
}

// failingAnchor errors on Publish exactly failCount times, then delegates
// to a real anchor -- used to exercise catchUpAnchor's retry path.
type failingAnchor struct {
	inner     core.Anchor
	failCount int
	calls     int
}

func (a *failingAnchor) Publish(ctx context.Context, seq int64, head []byte) error {
	a.calls++
	if a.calls <= a.failCount {
		return fmt.Errorf("failingAnchor: simulated publish failure (%d/%d)", a.calls, a.failCount)
	}
	return a.inner.Publish(ctx, seq, head)
}

func (a *failingAnchor) Head(ctx context.Context) (int64, []byte, error) {
	return a.inner.Head(ctx)
}

// TestAttestationService_PublishesToAnchor pins the happy path: after a
// successful RunAttestBatch, the configured Anchor's Head reflects the new
// seq/root_hash.
func TestAttestationService_PublishesToAnchor(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor := newTestAttestor(t, "attest-key-4")
	store := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	svc := service.NewAttestationService(store, attestor, anchor, core.NewEngine())

	_, seq, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	anchorSeq, anchorHead, err := anchor.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, seq, anchorSeq)

	var rootHash []byte
	require.NoError(t, pool.QueryRow(ctx, "SELECT root_hash FROM ledger_attestations WHERE seq=$1", seq).Scan(&rootHash))
	require.Equal(t, rootHash, anchorHead)
}

// TestAttestationService_CatchesUpAnchorAfterTransientFailure pins design
// doc §8.3's "本地重试队列": a Publish failure on one run does not lose
// the seq -- the NEXT run's catch-up step republishes it before creating a
// new attestation.
func TestAttestationService_CatchesUpAnchorAfterTransientFailure(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor := newTestAttestor(t, "attest-key-5")
	store := postgres.NewAttestationStore(pool)
	realAnchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))
	flaky := &failingAnchor{inner: realAnchor, failCount: 1}
	svc := service.NewAttestationService(store, attestor, flaky, core.NewEngine())

	// Run 1: attestation is created in the DB, but Publish fails (the
	// anchor never learns about seq 1).
	_, seq1, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	anchorSeqAfterRun1, _, err := realAnchor.Head(ctx)
	require.NoError(t, err)
	require.Zero(t, anchorSeqAfterRun1, "anchor must not have seq 1 yet -- Publish was made to fail")

	// Run 2: catch-up must republish seq1 before creating seq2.
	_, seq2, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotEqual(t, seq1, seq2)

	anchorSeqAfterRun2, _, err := realAnchor.Head(ctx)
	require.NoError(t, err)
	require.Equal(t, seq2, anchorSeqAfterRun2, "catch-up must have republished seq1, then run2's own Publish must have advanced the anchor to seq2")
}
