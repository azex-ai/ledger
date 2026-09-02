package service_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestVerifyLedger_SamplesTheNewestJournalsNotTheOldest pins step 4's
// sampling DIRECTION (2026-09-02 audit, tamper-evident.md M-1 / C-M1).
//
// The ledger gets 25 legitimately signed journals, then one forged row
// inserted by direct SQL -- the highest id in the table, carrying
// auth_status's column default ('unsigned_no_attestor'), which is exactly
// the shape an INSERT that never went through PostJournal produces.
//
// Step 4 samples cfg.JournalSampleSize (default 20) journals. Sampling the
// OLDEST 20 (what ListJournals with an empty cursor returns, which is what
// this step used to call) cannot reach id 26 at all, so the forged row was
// invisible and VerifyLedger reported VERIFIED. Sampling the NEWEST 20
// (core.JournalQuerier.ListRecentJournals) puts the forgery in the first
// position it looks at.
//
// Pinned symbol: service.VerifyLedger via
// core.JournalQuerier.ListRecentJournals (postgres.QueryStore).
func TestVerifyLedger_SamplesTheNewestJournalsNotTheOldest(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "verify-sampling-key")
	require.NoError(t, err)

	ledgerStore := postgres.NewLedgerStore(pool).WithAuth(attestor)
	attestStore := postgres.NewAttestationStore(pool)
	anchor := anchordev.NewLocalFileAnchorForDevelopment(filepath.Join(t.TempDir(), "anchor.txt"))
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	// 25 > the default sample size of 20, so the oldest page and the newest
	// page are disjoint enough for the direction to be observable.
	const legitimate = 25
	for i := 0; i < legitimate; i++ {
		_, err = ledgerStore.PostJournal(ctx, f.journalInput(int64(2000+i), postgrestest.UniqueKey(fmt.Sprintf("verify-sampling-%d", i))))
		require.NoError(t, err)
	}
	_, _, err = attestSvc.RunAttestBatch(ctx, 1000)
	require.NoError(t, err)

	// The forgery: a journal row with no signature, inserted straight into
	// the table. It deliberately carries no entries, so the only check that
	// can see it is step 4's per-journal signature sample -- this test is
	// about sampling direction, not about entry coverage (that is C-M2's
	// TestVerifyLedger_FlagsUncoveredUnsignedEntry).
	forgedID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("verify-sampling-forged"))
	var maxID int64
	require.NoError(t, pool.QueryRow(ctx, "SELECT max(id) FROM journals").Scan(&maxID))
	require.Equal(t, forgedID, maxID, "the forged journal must be the newest row for this test to mean anything")

	queries := postgres.NewQueryStore(pool)
	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.NotEqual(t, service.VerifyStatusVerified, report.Status,
		"a freshly forged, unsigned journal at the top of the table must not read as VERIFIED; report: %+v", report)
	require.Contains(t, fmt.Sprint(report.Reasons), "carry no signature",
		"the finding must name the unsigned journal(s) it found; reasons: %v", report.Reasons)
}

// TestListRecentJournals_ReturnsNewestFirst pins the adapter half directly:
// core.JournalQuerier.ListRecentJournals must order DESCENDING and must not
// be confused with ListJournals's ascending cursor walk. Without this,
// swapping the query back to ASC would only fail the (much more expensive)
// VerifyLedger pin above, with a failure message that points at
// verification rather than at the query.
//
// Pinned symbol: postgres.QueryStore.ListRecentJournals /
// postgres.QueryStore.ListJournals.
func TestListRecentJournals_ReturnsNewestFirst(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	ledgerStore := postgres.NewLedgerStore(pool)
	var uids []string
	for i := 0; i < 5; i++ {
		j, err := ledgerStore.PostJournal(ctx, f.journalInput(int64(3000+i), postgrestest.UniqueKey(fmt.Sprintf("recent-order-%d", i))))
		require.NoError(t, err)
		uids = append(uids, j.UID)
	}

	queries := postgres.NewQueryStore(pool)

	recent, err := queries.ListRecentJournals(ctx, 3)
	require.NoError(t, err)
	require.Len(t, recent, 3)
	require.Equal(t, []string{uids[4], uids[3], uids[2]}, []string{recent[0].UID, recent[1].UID, recent[2].UID},
		"ListRecentJournals must return the newest journals, newest first")

	oldest, _, err := queries.ListJournals(ctx, "", 3)
	require.NoError(t, err)
	require.Len(t, oldest, 3)
	require.Equal(t, uids[0], oldest[0].UID,
		"sanity: ListJournals with an empty cursor is still the OLDEST page -- that is why a second method exists")
}
