package service

import (
	"context"
	"strings"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestCheck2GlobalBalance_CallerCancellationIsIncompleteNotFailed pins I-M7
// (2026-09-02 audit) ②/④: when the caller's context ends before the scan
// finishes, the check reports Complete=false with a finding that blames the
// caller's deadline -- not Check2Timeout, which did not fire, and not
// Passed=false, which would read as an accounting violation.
func TestCheck2GlobalBalance_CallerCancellationIsIncompleteNotFailed(t *testing.T) {
	cls := &mockClassificationLister{
		classifications: []ClassificationDim{
			{ID: 10, UID: "cls-10", Code: "asset", NormalSide: core.NormalSideDebit},
		},
	}
	cpReader := &mockCheckpointReader{
		checkpoints: []BalanceCheckpoint{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 10, Balance: decimal.NewFromInt(100)},
		},
	}
	accountEntries := &mockAccountEntrySummer{
		debitByClass:  map[int64]decimal.Decimal{10: decimal.NewFromInt(100)},
		creditByClass: map[int64]decimal.Decimal{},
	}
	q := cleanQuerier()
	q.checkpointAccounts = []CheckpointAccountKey{
		{AccountHolder: 1, CurrencyID: 1},
		{AccountHolder: 2, CurrencyID: 1},
	}
	svc := buildFullSvcForCheck2(t, accountEntries, cpReader, cls, q, FullReconciliationConfig{
		Check2ScanLimit: 100,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller is already gone when the check starts

	result := svc.runCheck2GlobalBalance(ctx)
	require.False(t, result.Complete, "a scan the caller cancelled must not claim coverage")
	require.True(t, result.Passed, "cancellation is not an accounting finding: %+v", result.Findings)
	var named bool
	for _, f := range result.Findings {
		if strings.Contains(f.Description, "caller context ended") {
			named = true
		}
	}
	require.True(t, named, "the finding must name the caller's deadline, not Check2Timeout: %+v", result.Findings)
}
