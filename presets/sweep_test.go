package presets

import (
	"context"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSweepLifecycle_Validate(t *testing.T) {
	assert.NoError(t, SweepLifecycle.Validate())
}

func TestSweepLifecycle_Transitions(t *testing.T) {
	lc := SweepLifecycle

	tests := []struct {
		name string
		from core.Status
		to   core.Status
		want bool
	}{
		{"pending -> sent", "pending", "sent", true},
		{"pending -> confirmed (must go through sent)", "pending", "confirmed", false},
		{"sent -> confirmed", "sent", "confirmed", true},
		{"sent -> failed", "sent", "failed", true},
		{"failed -> pending (retry)", "failed", "pending", true},
		{"confirmed -> anything (terminal)", "confirmed", "pending", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}

func TestInstallSweepClassification_CreatesOnce(t *testing.T) {
	ctx := context.Background()
	store := newFakeClassificationStore()

	created, err := InstallSweepClassification(ctx, store)
	require.NoError(t, err)
	assert.Equal(t, SweepClassificationCode, created.Code)
	assert.True(t, created.IsSystem)
	assert.Equal(t, core.NormalSideCredit, created.NormalSide)
	require.NotNil(t, created.Lifecycle)
	assert.Equal(t, SweepLifecycle, created.Lifecycle)
	assert.Equal(t, core.BalanceRoleNone, created.BalanceRole)

	// Idempotent: a second call returns the same row, not an error.
	again, err := InstallSweepClassification(ctx, store)
	require.NoError(t, err)
	assert.Equal(t, created.UID, again.UID)
}

func TestInstallSweepClassification_RejectsExistingWithoutLifecycle(t *testing.T) {
	ctx := context.Background()
	store := newFakeClassificationStore()

	// Simulate a pre-existing "sweep" classification that some other caller
	// created label-only (no lifecycle) -- this must be surfaced as a
	// conflict, not silently accepted.
	_, err := store.CreateClassification(ctx, core.ClassificationInput{
		Code:       SweepClassificationCode,
		Name:       "Crypto Sweep",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
	})
	require.NoError(t, err)

	_, err = InstallSweepClassification(ctx, store)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrInvalidInput)
}

func TestInstallCryptoDepositBundle(t *testing.T) {
	ctx := context.Background()
	classStore := newFakeClassificationStore()
	journalStore := newFakeJournalTypeStore()
	templateStore := newFakeTemplateStore()

	err := InstallCryptoDepositBundle(ctx, classStore, journalStore, templateStore)
	require.NoError(t, err)

	// Deposit bundle classifications are present.
	depositClass, err := classStore.GetByCode(ctx, "pending")
	require.NoError(t, err)
	assert.Equal(t, core.BalanceRolePending, depositClass.BalanceRole)

	// Sweep classification is present and lifecycle-bearing.
	sweepClass, err := classStore.GetByCode(ctx, SweepClassificationCode)
	require.NoError(t, err)
	require.NotNil(t, sweepClass.Lifecycle)

	// Sweep never gets a journal template -- no journal type named after it.
	_, err = journalStore.GetJournalTypeByCode(ctx, SweepClassificationCode)
	assert.ErrorIs(t, err, core.ErrNotFound)

	// Safe to call twice.
	require.NoError(t, InstallCryptoDepositBundle(ctx, classStore, journalStore, templateStore))
}
