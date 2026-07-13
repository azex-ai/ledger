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
// Retry: failed -> pending, same booking (service.Onchain.reviveFailedSweep) --
// gas-bump exhaustion re-requests a fresh signer nonce rather than reusing
// the stale one (which the signer EOA may still report as "next" forever),
// but stays on this same booking UID/idempotency key rather than minting a
// new one, so there is exactly one audit trail per (chain,token) sweep saga.
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

// DepositClassificationCode is the Classification.Code deposit bookings are
// created against (CreateBookingInput.ClassificationCode) -- distinct from
// the accounting-side classifications DepositBundle() installs (main_wallet,
// suspense, custodial, pending), which are referenced by journal entry
// templates, not by Booker.
const DepositClassificationCode = "deposit"

// InstallDepositClassification idempotently creates (or validates) the
// "deposit" classification carrying DepositLifecycle, mirroring
// InstallSweepClassification. This is a separate install step from
// DepositBundle()'s accounting classifications: ClassificationPreset (the
// type DepositBundle's Classifications are built from) has no Lifecycle
// field, so none of main_wallet/suspense/custodial/pending are ever
// booking-capable -- a "deposit" booking needs this classification to exist
// before Booker.CreateBooking(ClassificationCode: "deposit", ...) can work.
// NormalSide is inert here (fixed to NormalSideCredit by convention, same as
// InstallSweepClassification): no journal entry is ever posted directly
// against classification code "deposit" itself -- entries post against
// main_wallet/custodial/etc via templates, cross-linked through EventUID.
//
// A "deposit" classification row already exists on every install (migration
// 011 seeds it, predating the lifecycle column) but with NO lifecycle --
// this function backfills it via SetLifecycleIfEmpty (expand-safe: never
// clobbers a lifecycle an operator has since customized) rather than trying
// to CreateClassification, which would conflict on the unique code.
func InstallDepositClassification(ctx context.Context, classifications core.ClassificationStore) (*core.Classification, error) {
	existing, err := classifications.GetByCode(ctx, DepositClassificationCode)
	if err == nil {
		if !existing.IsActive {
			return nil, fmt.Errorf("presets: existing deposit classification is inactive: %w", core.ErrInvalidInput)
		}
		if existing.Lifecycle == nil {
			if err := classifications.SetLifecycleIfEmpty(ctx, existing.UID, DepositLifecycle); err != nil {
				return nil, fmt.Errorf("presets: backfill deposit classification lifecycle: %w", err)
			}
			existing.Lifecycle = DepositLifecycle
		}
		return existing, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return nil, fmt.Errorf("presets: get deposit classification: %w", err)
	}

	created, err := classifications.CreateClassification(ctx, core.ClassificationInput{
		Code:       DepositClassificationCode,
		Name:       "Deposit",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  DepositLifecycle,
	})
	if err != nil {
		return nil, fmt.Errorf("presets: create deposit classification: %w", err)
	}
	return created, nil
}

// InstallCryptoDepositBundle installs the standard deposit accounting bundle
// (DepositBundle), the deposit booking lifecycle classification, and the
// sweep booking lifecycle. Sweep intentionally carries no journal templates
// -- it never touches the accounting equation (design doc §4: "sweep 只走
// booking + event，无 journal"). Safe to call repeatedly.
func InstallCryptoDepositBundle(
	ctx context.Context,
	classifications core.ClassificationStore,
	journalTypes core.JournalTypeStore,
	templates core.TemplateStore,
) error {
	if err := InstallTemplateBundle(ctx, classifications, journalTypes, templates, DepositBundle()); err != nil {
		return err
	}
	if _, err := InstallDepositClassification(ctx, classifications); err != nil {
		return err
	}
	if _, err := InstallSweepClassification(ctx, classifications); err != nil {
		return err
	}
	return nil
}
