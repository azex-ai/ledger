package service_test

// I-28's mechanism had no pin (2026-09-03 W5 mutation survey). The section
// claims "the latest external anchor head matches the DB's attestation
// chain", and its enforcement is one comparison in VerifyLedger:
//
//	if a.Seq == anchorSeq && !bytes.Equal(a.RootHash, anchorHead) {
//	    tampered("seq %d: DB root_hash does not match the externally anchored head", a.Seq)
//	}
//
// Disabling that line left `go test ./...` -- every package, postgres
// included -- entirely green. All eleven pins the section listed are about
// the cases AROUND it: an empty anchor, an anchor that lags, an anchor that
// rolled back, an anchor ahead of the DB. Each of those is decided by a
// different branch (the ones below the loop, or the anchor_observations
// memory), and every one of them still passed with the head comparison
// gone.
//
// That is the whole point of anchoring. Every other check in VerifyLedger
// reads data an attacker who owns the database can rewrite; this one is the
// only comparison against something outside it.

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// TestVerifyLedger_AnchorHeadDisagreeingAtTheAnchoredSeqIsTampered is the
// isolating pin: everything in the database is genuine and internally
// consistent, and the anchor holds a different root for the seq it says it
// anchored.
//
// Isolating matters here. Editing the DB row would also trip the root_hash
// self-consistency check one branch above, so a report of TAMPERED would
// prove nothing about the anchor comparison -- which is exactly how this
// mechanism came to have no pin. Changing what the ANCHOR says leaves every
// DB-side check passing, so the finding can only come from the comparison
// under test.
func TestVerifyLedger_AnchorHeadDisagreeingAtTheAnchoredSeqIsTampered(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	f := setupAttestFixture(t, pool, ctx)

	attestor, verifier, err := ed25519KeyPair(t, "anchor-head-pin-key")
	require.NoError(t, err)
	attestStore := postgres.NewAttestationStore(pool)

	anchorPath := filepath.Join(t.TempDir(), "anchor.txt")
	anchor := anchordev.NewLocalFileAnchorForDevelopment(anchorPath)
	attestSvc := service.NewAttestationService(attestStore, attestor, nil, anchor, core.NewEngine())

	journalID := insertForgedJournal(t, ctx, pool, f, postgrestest.UniqueKey("anchor-head-pin"))
	tx := beginWithCleanup(t, ctx, pool)
	insertBalancedPairInTx(t, ctx, tx, f, journalID, 9801, 9802)
	require.NoError(t, tx.Commit(ctx))

	_, seq, err := attestSvc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	require.NotZero(t, seq)

	queries := postgres.NewQueryStore(pool)

	want := fmt.Sprintf("seq %d: DB root_hash does not match the externally anchored head", seq)

	// Control: with the anchor holding what was published, this finding is
	// absent. Stated as the presence of one REASON rather than as an
	// overall VERIFIED, because the fixture's journal is deliberately
	// forged (that is how the entries get there without a write path) and
	// so carries an unsigned-journal finding of its own. Comparing statuses
	// would make the assertion below pass on that unrelated finding.
	clean := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.NotContainsf(t, clean.Reasons, want,
		"before any tampering, the anchor-head comparison must have nothing to say: %v", clean.Reasons)

	// Now the carrier says something else for the same seq. This is the
	// shape of a compromised or swapped anchor -- and equally of a database
	// rolled back and re-signed with a leaked key, which is the same
	// disagreement seen from the other side.
	rewriteLocalAnchorHead(t, anchorPath, seq, strings.Repeat("ab", 32))

	report := service.VerifyLedger(ctx, attestStore, anchor, verifier, queries, service.VerifyConfig{})
	require.Equalf(t, service.VerifyStatusTampered, report.Status,
		"the DB chain and the external anchor disagree at seq %d and VerifyLedger reported %s. Every other check in "+
			"this function reads data an attacker who owns the database can rewrite; this comparison is the only one "+
			"against something outside it, and it is what anchoring is for. Reasons: %v",
		seq, report.Status, report.Reasons)
	require.Containsf(t, report.Reasons, want,
		"the finding must be the anchor-head comparison itself, not a side effect of some other check: %v", report.Reasons)
}

// rewriteLocalAnchorHead overwrites the dev anchor's file directly, which is
// how an out-of-band write to the carrier is modelled -- Publish is
// create-only per seq, deliberately (I-56), so the anchor's own API cannot
// produce this state.
func rewriteLocalAnchorHead(t *testing.T, path string, seq int64, hexRoot string) {
	t.Helper()
	_, err := hex.DecodeString(hexRoot)
	require.NoError(t, err, "the replacement head must be valid hex, or the read fails for the wrong reason")
	require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf("%d\n%s\n", seq, hexRoot)), 0o600))
}
