package postgres_test

// Pin tests for T4: attestation-batched authorization verdicts
// (docs/plans/2026-08-21-tamper-evident-ledger-design.md §8 extended,
// docs/plans/2026-08-21-integrity-hardening-contracts.md §W3-B,
// docs/INVARIANTS.md I-33). These exercise the fast READ side --
// postgres.VerifiedBalanceStore trusting a cached
// core.JournalAuthVerdict without a live core.VerifyJournalAuth call.
// service/attestation_auth_verdict_test.go exercises the WRITE side
// (RunAttestBatch computing and caching verdicts) and the periodic-verify
// drift check.
import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck is the
// pin for I-33's one-directional trust rule: a cached
// core.JournalAuthVerdictAuthorized is NOT a substitute for verification.
//
// After RunAttestBatch caches Authorized for a genuinely-signed journal, this
// test corrupts that journal's stored signature directly via SQL (an
// owner-role bypass of the no-arbitrary-update trigger -- this wave's
// standing threat model) so that a LIVE re-verification would now fail. The
// sanity check below confirms it really would, so the final assertion tests
// the gate and not the fixture (working-agreements §3). VerifiedBalance must
// then REFUSE (core.ErrUnauthorizedJournal): the cached verdict answers "was
// this journal authorized when it was attested", and a withdrawal gate needs
// "is it authorized now".
//
// T4 originally asserted the opposite here -- the design was wrong, not the
// test. The body carries the full account at the assertion itself.
func TestVerifiedBalance_CachedAuthorizedVerdictDoesNotSkipTheLiveCheck(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9101

	attestor, verifier := newTestAttestor(t, "ed25519-t4-cached-authorized")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	journal, err := ledgerStore.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("t4-cached-authorized"), decimal.NewFromInt(250)))
	require.NoError(t, err)

	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())
	attested, _, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested)

	// Corrupt the journal's stored signature -- content a live
	// core.VerifyJournalAuth call would now reject.
	_, err = pool.Exec(ctx, "ALTER TABLE journals DISABLE TRIGGER journals_no_arbitrary_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE journals SET auth_signature = decode(repeat('ff', 32), 'hex') WHERE uid = $1", journal.UID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journals ENABLE TRIGGER journals_no_arbitrary_update")
	require.NoError(t, err)

	// Sanity (working-agreements §3: falsification evidence, not just a
	// green test): a live re-check on the now-corrupted journal really
	// would fail -- this is what the naive, pre-T4 path would have hit on
	// every single call.
	corruptedJournal, entries, err := postgres.NewQueryStore(pool).GetJournal(ctx, journal.UID)
	require.NoError(t, err)
	corruptedInput := core.JournalInputFromRecord(*corruptedJournal, entries)
	sanityErr := core.VerifyJournalAuth(ctx, verifier, corruptedInput, corruptedJournal.EffectiveAt, corruptedJournal.AuthDigest, corruptedJournal.AuthSignature, corruptedJournal.AuthKeyID)
	require.Error(t, sanityErr, "test setup: a live re-verification of the corrupted journal must fail, or this test proves nothing")
	require.ErrorIs(t, sanityErr, core.ErrUnauthorizedJournal)

	// This assertion is the inverse of what it used to be, and the reason is
	// worth stating where the test lives.
	//
	// T4 cached the verdict so a later VerifiedBalance could skip the live
	// check, and this test pinned exactly that: it corrupted the signature,
	// proved a live check would now fail, and then required VerifiedBalance to
	// succeed anyway. The test was correct about the design. The design was
	// wrong.
	//
	// A cached Authorized answers "was this journal authorized when it was
	// attested". The withdrawal gate needs "is it authorized now", and those
	// differ for exactly the reason the sanity check above demonstrates:
	// something changed after attestation. The cached verdict was described as
	// being "at least as strict as a live check" (I-33), and it is not -- it is
	// laxer, by precisely the window between attestation and read.
	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	_, err = vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err, "a cached Authorized verdict must not excuse a journal whose live verification now fails")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
}

// TestVerifiedBalance_RefusesTamperedEntryAmount is the case the gate exists
// for, and the one the cached fast path let through.
//
// The test above corrupts a signature, which is a tell -- nobody gains money
// from an invalid signature. This one edits the amount an entry contributes,
// which is what an attacker with database write access actually wants: the
// balance goes up, and every layer above still reports health. The journal's
// cached verdict stays Authorized, because it was authorized when attested.
// core.CanonicalJournalDigest covers each entry's Amount, so a live check
// notices; skipping it on the cached verdict did not.
//
// This asserts the balance is refused rather than merely different, because
// "different" would still be a number a withdrawal could be paid against.
func TestVerifiedBalance_RefusesTamperedEntryAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9142

	attestor, verifier := newTestAttestor(t, "ed25519-t4-tampered-amount")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	journal, err := ledgerStore.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("t4-tampered-amount"), decimal.NewFromInt(250)))
	require.NoError(t, err)

	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())
	attested, _, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested)

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	before, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.NoError(t, err, "the untampered journal must verify")
	require.True(t, before.Equal(decimal.NewFromInt(250)))

	// Double both legs so the journal still balances per currency -- a
	// single-sided edit would trip the balance trigger and never reach the
	// question this test is asking.
	internalID := postgrestest.InternalID(t, pool, "journals", journal.UID)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries DISABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "UPDATE journal_entries SET amount = amount * 2 WHERE journal_id = $1", internalID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE journal_entries ENABLE TRIGGER journal_entries_no_update")
	require.NoError(t, err)

	// The cached verdict is untouched and still says Authorized -- the edit
	// happened after attestation, which is the whole point.
	var verdict string
	require.NoError(t, pool.QueryRow(ctx,
		`SELECT DISTINCT auth_verdict FROM entry_attestations ea
		   JOIN journal_entries e ON e.id = ea.entry_id
		  WHERE e.journal_id = $1`, internalID).Scan(&verdict))
	require.Equal(t, string(core.JournalAuthVerdictAuthorized), verdict,
		"test setup: the cached verdict must still read Authorized, or this test is not exercising the gap")

	_, err = vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err,
		"an entry edited after attestation must make the balance UNDEFINED -- returning 500 here would let a withdrawal be paid against a number nobody signed")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
}

// TestVerifiedBalance_CachedUnauthorizedVerdictIsUndefinedWithoutLiveVerifier
// proves the cached-verdict fast path is fully decoupled from having a
// live core.AuthVerifier at READ time: a forged journal's verdict, cached
// as Unauthorized when RunAttestBatch ran (with a real verifier
// configured for the ATTESTATION worker), still makes VerifiedBalance
// UNDEFINED even when the VerifiedBalanceStore doing the reading has NO
// verifier at all -- proving the rejection came from the cache, not from
// the nil-verifier bailout (which produces a different error message,
// asserted against below).
func TestVerifiedBalance_CachedUnauthorizedVerdictIsUndefinedWithoutLiveVerifier(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9102

	attestor, verifier := newTestAttestor(t, "ed25519-t4-cached-unauthorized")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledgerStore.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("t4-cached-unauthorized-signed"), decimal.NewFromInt(100)))
	require.NoError(t, err)
	insertForgedBalancedJournal(t, pool, ctx, f, holder, "1000000.000000000000000000", postgrestest.UniqueKey("t4-cached-unauthorized-forged"))

	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())
	attested, _, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 4, attested)

	// Read with NO verifier at all -- the cached verdict alone must still
	// reject.
	vb := postgres.NewVerifiedBalanceStore(pool, nil)
	balance, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err)
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
	require.Contains(t, err.Error(), "cached attestation verdict is unauthorized",
		"the rejection must come from the cached-verdict fast path, not the separate nil-AuthVerifier bailout")
	require.True(t, balance.IsZero())
}

// TestVerifiedBalance_UnattestedForgedTailJournalStillCaughtAlongsideCachedAuthorized
// pins the mixed case: one journal is fully attested and cached as
// Authorized (trusted, zero extra work); a SECOND, forged journal on the
// SAME dimension has never been attested at all (the tail). VerifiedBalance
// must still perform a live check on the tail journal and reject the whole
// balance -- a cached-authorized entry for one journal must never mask an
// unattested, unauthorized journal contributing to the same dimension.
func TestVerifiedBalance_UnattestedForgedTailJournalStillCaughtAlongsideCachedAuthorized(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupVBFixture(t, pool, ctx)
	const holder int64 = 9103

	attestor, verifier := newTestAttestor(t, "ed25519-t4-mixed-tail")
	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	_, err := ledgerStore.PostJournal(ctx, f.journalInput(holder, postgrestest.UniqueKey("t4-mixed-tail-signed"), decimal.NewFromInt(100)))
	require.NoError(t, err)

	attestStore := postgres.NewAttestationStore(pool)
	attestSvc := service.NewAttestationService(attestStore, attestor, verifier, nil, core.NewEngine())
	attested, _, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.Equal(t, 2, attested, "the signed journal must be attested (and cached Authorized) before the forged one is even inserted")

	// Forged journal inserted AFTER the attestation run -- it is genuinely
	// in the uncovered tail, never attested at all.
	insertForgedBalancedJournal(t, pool, ctx, f, holder, "1000000.000000000000000000", postgrestest.UniqueKey("t4-mixed-tail-forged"))

	vb := postgres.NewVerifiedBalanceStore(pool, verifier)
	balance, err := vb.VerifiedBalance(ctx, holder, f.CurrencyUID, f.AvailableUID)
	require.Error(t, err, "an unattested forged journal must still be caught even though another contributing journal has a cached Authorized verdict")
	require.ErrorIs(t, err, core.ErrUnauthorizedJournal)
	require.True(t, balance.IsZero())
}
