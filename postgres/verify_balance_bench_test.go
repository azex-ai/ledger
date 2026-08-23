package postgres_test

// W2-T3 (bus board #17, only-read measurement task,
// docs/plans/2026-08-21-integrity-hardening-contracts.md W2-2): measures
// the naive "verify this account's balance" path -- discover every journal
// that ever touched the account's dimension, reconstruct each journal's
// core.JournalInput from the DB (never trusting the in-process value), and
// run core.VerifyJournalAuth on each one -- at realistic entry-count scale
// (N = 100 / 1k / 10k / 100k), plus core.CheckpointIntegrityStore
// .RecomputeBalance's own isolated cost at the same scale (P2's
// entries-only rescan a verified balance would sit on top of, per T2's
// port design).
//
// This is a Benchmark, not a Test -- `go test ./...` (what `make test` runs)
// never executes it: Benchmark functions only run when explicitly selected
// via `-bench`, with no need for a `-short` guard (an earlier version of
// this file used `testing.Short()` on a plain Test, which does not help
// here because the project's `make test` target does not pass `-short` --
// that version ran unconditionally inside the regular suite and stretched
// it from ~2 minutes to 20+, see bus checkpoint 2026-08-23). It does not
// loop via b.Loop()/b.N: at N=100k each measurement point is already a
// single, multi-second-to-minutes operation, not something to multiply by
// an auto-scaled iteration count. It seeds one shared account CUMULATIVELY
// across tiers (100 -> 1,000 -> 10,000 -> 100,000 total entries, each tier
// only adding the delta) rather than reseeding every tier from zero, and
// reports wall-clock via b.Logf (shown with -v) so the raw numbers land
// directly in the invocation's output.
//
// Run (needs Docker; long -- budget several minutes for the 100k tier):
//
//	go test ./postgres/ -bench=BenchmarkVerifyBalance_NaivePath -run=^$ -benchtime=1x -v -timeout 30m
//
// Reuses unexported helpers from auth_pin_test.go (authFixture,
// setupAuthFixture, newTestAttestor) and sqlcgen.ListEntriesByAccount --
// same package (postgres_test), no existing file touched.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// discoverJournalUIDsForAccount pages through ListEntriesByAccount (the
// same query a real "verify this account" caller would use to discover
// which journals contributed to a dimension) and returns the distinct
// journal uids touching it, oldest first.
func discoverJournalUIDsForAccount(t testing.TB, pool *pgxpool.Pool, ctx context.Context, holder, currencyID int64) []string {
	t.Helper()
	q := sqlcgen.New(pool)
	const pageSize = int32(1000)

	var uids []string
	seen := make(map[string]struct{})
	var cursor int64
	for {
		rows, err := q.ListEntriesByAccount(ctx, sqlcgen.ListEntriesByAccountParams{
			AccountHolder: holder,
			CurrencyID:    currencyID,
			CursorID:      cursor,
			PageLimit:     pageSize,
		})
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		for _, r := range rows {
			if r.ID.Valid && r.ID.Int64 > cursor {
				cursor = r.ID.Int64
			}
			juid := uuid.UUID(r.JournalUid.Bytes).String()
			if _, ok := seen[juid]; !ok {
				seen[juid] = struct{}{}
				uids = append(uids, juid)
			}
		}
		if int32(len(rows)) < pageSize {
			break
		}
	}
	return uids
}

// reconstructJournalForVerify re-derives everything core.VerifyJournalAuth
// needs for one journal from the database in a single round trip (a
// journals/journal_entries join by uid) -- it deliberately does NOT reuse
// any in-process core.JournalInput a caller might already be holding,
// because a real verify-balance caller (a withdrawal gate, reconcile,
// ledger-cli verify) has no such in-memory value; it only has what is
// persisted. classUIDByID/currencyUID are pre-resolved once by the caller
// (this bench fixture uses exactly one currency and two classifications
// for its entire run) to isolate the signature-verification cost from
// dimension-resolution cost, which in production is what postgres.dimCache
// already amortizes to ~zero once warm -- see the report's caveats.
func reconstructJournalForVerify(t testing.TB, pool *pgxpool.Pool, ctx context.Context, journalUID string, journalTypeUID, currencyUID string, currencyID int64, classUIDByID map[int64]string) (core.JournalInput, time.Time, []byte, []byte, string) {
	t.Helper()

	rows, err := pool.Query(ctx, `
		SELECT j.idempotency_key, j.actor_id, j.source, j.effective_at,
		       j.auth_digest, j.auth_signature, j.auth_key_id,
		       je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount::text
		FROM journals j
		JOIN journal_entries je ON je.journal_id = j.id
		WHERE j.uid = $1
		ORDER BY je.id`, journalUID)
	require.NoError(t, err)
	defer rows.Close()

	var (
		idemKey, source, keyID string
		actorID                int64
		effectiveAt            time.Time
		digest, signature      []byte
		entries                []core.EntryInput
	)
	for rows.Next() {
		var holder, rowCurrencyID, classID int64
		var entryType, amountStr string
		require.NoError(t, rows.Scan(
			&idemKey, &actorID, &source, &effectiveAt,
			&digest, &signature, &keyID,
			&holder, &rowCurrencyID, &classID, &entryType, &amountStr,
		))
		require.Equal(t, currencyID, rowCurrencyID, "bench fixture posts a single currency; unexpected currency_id in row")
		amt, err := decimal.NewFromString(amountStr)
		require.NoError(t, err)
		classUID, ok := classUIDByID[classID]
		require.True(t, ok, "unresolved classification_id %d -- bench fixture's dim map is incomplete", classID)
		entries = append(entries, core.EntryInput{
			AccountHolder:     holder,
			CurrencyUID:       currencyUID,
			ClassificationUID: classUID,
			EntryType:         core.EntryType(entryType),
			Amount:            amt,
		})
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, entries, "journal %s: no entries found (join returned zero rows)", journalUID)

	input := core.JournalInput{
		JournalTypeUID: journalTypeUID,
		IdempotencyKey: idemKey,
		ActorID:        actorID,
		Source:         source,
		Entries:        entries,
	}
	return input, effectiveAt, digest, signature, keyID
}

// BenchmarkVerifyBalance_NaivePath is the report-generating measurement for
// W2-T3 items 2 and 4. It is intentionally verbose in its b.Logf output --
// the report file (.local/bench-verify-2026-08-23.md) quotes this output
// verbatim rather than re-deriving numbers by hand.
func BenchmarkVerifyBalance_NaivePath(b *testing.B) {
	pool := postgrestest.SetupDB(b)
	ctx := context.Background()
	f := setupAuthFixture(b, pool, ctx)
	attestor, verifier := newTestAttestor(b, "verify-balance-bench-key")
	store := postgres.NewLedgerStore(pool).WithAuth(attestor)
	integrityStore := postgres.NewCheckpointIntegrityStore(pool)

	classUIDByID := map[int64]string{}
	// Populated lazily below once we know the ids (setupAuthFixture keeps
	// them private; re-derive via the same lookup pattern it uses).
	var mainWalletID, custodialID int64
	require.NoError(b, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", f.MainWalletUID).Scan(&mainWalletID))
	require.NoError(b, pool.QueryRow(ctx, "SELECT id FROM classifications WHERE uid=$1", f.CustodialUID).Scan(&custodialID))
	classUIDByID[mainWalletID] = f.MainWalletUID
	classUIDByID[custodialID] = f.CustodialUID
	var currencyID int64
	require.NoError(b, pool.QueryRow(ctx, "SELECT id FROM currencies WHERE uid=$1", f.CurrencyUID).Scan(&currencyID))

	const userID int64 = 424242
	// Cumulative deltas: 100, then +900 (=1,000), then +9,000 (=10,000),
	// then +90,000 (=100,000). Each tier's measurement runs against
	// everything seeded so far -- no reseed-from-zero between tiers.
	deltas := []int{100, 900, 9_000, 90_000}
	targets := []int{100, 1_000, 10_000, 100_000}

	seedWallStart := time.Now()
	cumulative := 0
	for tier, delta := range deltas {
		tierSeedStart := time.Now()
		for i := range delta {
			idem := postgrestest.UniqueKey(fmt.Sprintf("verifybal-t%d-%d", tier, i))
			_, err := store.PostJournal(ctx, f.journalInput(userID, idem, decimal.NewFromInt(1)))
			require.NoError(b, err)
		}
		cumulative += delta
		require.Equal(b, targets[tier], cumulative, "cumulative entry count drifted from the intended tier target")
		tierSeedElapsed := time.Since(tierSeedStart)
		totalSeedElapsed := time.Since(seedWallStart)

		// --- Phase A: discovery (ListEntriesByAccount pagination).
		discoverStart := time.Now()
		journalUIDs := discoverJournalUIDsForAccount(b, pool, ctx, userID, currencyID)
		discoverElapsed := time.Since(discoverStart)
		require.Len(b, journalUIDs, cumulative, "discovered journal count must equal seeded entry count (1 entry per journal on this account in this fixture)")

		// --- Phase B: per-journal reconstruct + core.VerifyJournalAuth.
		verifyStart := time.Now()
		for _, juid := range journalUIDs {
			input, effectiveAt, digest, signature, keyID := reconstructJournalForVerify(b, pool, ctx, juid, f.JournalTypeUID, f.CurrencyUID, currencyID, classUIDByID)
			require.NoError(b, core.VerifyJournalAuth(ctx, verifier, input, effectiveAt, digest, signature, keyID))
		}
		verifyAllElapsed := time.Since(verifyStart)

		// --- Phase C: core.CheckpointIntegrityStore.RecomputeBalance's own
		// isolated cost at this N, 5 reps for a mean (no signature
		// verification at all -- this is the "floor" verify-balance would
		// sit on top of per W2-2's framing).
		const recomputeReps = 5
		recomputeStart := time.Now()
		var lastBalance decimal.Decimal
		for range recomputeReps {
			bal, err := integrityStore.RecomputeBalance(ctx, userID, f.CurrencyUID, f.MainWalletUID)
			require.NoError(b, err)
			lastBalance = bal
		}
		recomputeElapsed := time.Since(recomputeStart)
		recomputeMean := recomputeElapsed / recomputeReps

		b.Logf(
			"N=%d | tier_seed=%s cumulative_seed_wall=%s | discover=%s (%s/entry) | verify_all=%s (%s/journal) | recompute_balance_mean=%s (reps=%d) | balance=%s",
			cumulative,
			tierSeedElapsed, totalSeedElapsed,
			discoverElapsed, discoverElapsed/time.Duration(cumulative),
			verifyAllElapsed, verifyAllElapsed/time.Duration(cumulative),
			recomputeMean, recomputeReps,
			lastBalance.String(),
		)
	}
}
