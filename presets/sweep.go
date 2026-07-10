package presets

import (
	"context"
	"errors"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// SweepLifecycle is a preset classification lifecycle for the crypto sweep
// booking (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §4). A sweep
// booking never posts a journal -- its only purpose is idempotency (booking
// key = sweep-{chain_id}-{token}-{signer_nonce}) and an audit trail for one
// batch collection transaction.
// States: pending -> sent -> confirmed | failed
// Retry: failed -> pending (same nonce, gas-bump reuses the same booking).
var SweepLifecycle = &core.Lifecycle{
	Initial:  "pending",
	Terminal: []core.Status{"confirmed"},
	Transitions: map[core.Status][]core.Status{
		"pending": {"sent"},
		"sent":    {"confirmed", "failed"},
		"failed":  {"pending"},
	},
}

// SweepClassificationCode is the Classification.Code sweep bookings are
// created against (CreateBookingInput.ClassificationCode).
const SweepClassificationCode = "sweep"

// InstallSweepClassification idempotently creates (or validates) the "sweep"
// classification carrying SweepLifecycle. Unlike the accounting bundles
// below, sweep has no BalanceRole and is never referenced by an
// EntryTemplate: NormalSide is therefore inert (no journal entry is ever
// posted against this classification) and fixed to NormalSideCredit by
// convention, matching this package's other IsSystem-only classifications
// (e.g. custodial).
func InstallSweepClassification(ctx context.Context, classifications core.ClassificationStore) (*core.Classification, error) {
	existing, err := classifications.GetByCode(ctx, SweepClassificationCode)
	if err == nil {
		if !existing.IsActive {
			return nil, fmt.Errorf("presets: existing sweep classification is inactive: %w", core.ErrInvalidInput)
		}
		if !existing.IsSystem {
			return nil, fmt.Errorf("presets: existing sweep classification has is_system=false, want true: %w", core.ErrInvalidInput)
		}
		if existing.Lifecycle == nil {
			return nil, fmt.Errorf("presets: existing sweep classification has no lifecycle: %w", core.ErrInvalidInput)
		}
		return existing, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("presets: get sweep classification: %w", err)
	}

	created, err := classifications.CreateClassification(ctx, core.ClassificationInput{
		Code:       SweepClassificationCode,
		Name:       "Crypto Sweep",
		NormalSide: core.NormalSideCredit,
		IsSystem:   true,
		Lifecycle:  SweepLifecycle,
	})
	if err != nil {
		return nil, fmt.Errorf("presets: create sweep classification: %w", err)
	}
	return created, nil
}

// InstallCryptoDepositBundle installs the standard deposit accounting bundle
// (DepositBundle) plus the sweep booking lifecycle. Sweep intentionally
// carries no journal templates -- it never touches the accounting equation
// (design doc §4: "sweep 只走 booking + event，无 journal"). Safe to call
// repeatedly.
func InstallCryptoDepositBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	if err := InstallTemplateBundle(ctx, classifications, journalTypes, templates, DepositBundle()); err != nil {
		return err
	}
	if _, err := InstallSweepClassification(ctx, classifications); err != nil {
		return err
	}
	return nil
}
