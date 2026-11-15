package main

import (
	"context"
	"fmt"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

const chargeTemplate = "signed_credit_spend"

// usageEvent represents an already submitted, billable usage result owned by
// the host. It is not a saved financial form or a reusable authorization draft.
// The host keeps this payload and its reservation association immutable across
// delivery retries, including Partial and RecordedAt.
type usageEvent struct {
	ID         string
	Amount     decimal.Decimal
	Partial    bool
	RecordedAt time.Time
}

// capture composes the existing APIs; it is deliberately example-local.
// reservation must come from Reserve / the host's authenticated job record,
// never from holder/currency fields supplied by an untrusted client. The host
// owns cross-event and cross-operation deduplication, and controls this template.
func capture(ctx context.Context, svc *ledger.Service, reservation *core.Reservation, event usageEvent) (*core.Journal, error) {
	if reservation == nil || reservation.UID == "" || event.ID == "" || event.RecordedAt.IsZero() {
		return nil, fmt.Errorf("signed capture: reservation and submitted event required: %w", core.ErrInvalidInput)
	}
	// These two keys are derived once from the same durable event identity.
	// A lost response retries the entire composition with BOTH unchanged.
	chargeKey := "signed-capture:charge:" + event.ID
	settleKey := "signed-capture:settle:" + event.ID
	request := core.TemplateExecutionRequest{
		TemplateCode: chargeTemplate,
		Params: core.TemplateParams{
			HolderID: reservation.AccountHolder, CurrencyUID: reservation.CurrencyUID,
			IdempotencyKey: chargeKey, EffectiveAt: event.RecordedAt,
			Source:  "signed-capture-example",
			Amounts: map[string]decimal.Decimal{"amount": event.Amount},
		},
	}

	// Both signing and verification may be remote. Finish them on the top-level
	// Service BEFORE RunInTx. Retain this prepared value only for this attempt.
	authorized, err := svc.AuthorizeTemplate(ctx, request.TemplateCode, request.Params)
	if err != nil {
		return nil, fmt.Errorf("authorize charge: %w", err)
	}
	if authorized.Status != core.AuthStatusSigned || svc.AuthVerifier() == nil {
		return nil, fmt.Errorf("signed capture: signing and verification required: %w", core.ErrUnauthorizedJournal)
	}
	if len(authorized.Digest) == 0 && len(authorized.Signature) == 0 && authorized.KeyID == "" {
		// Authorize's existing-key optimization returns the stored status but
		// no new auth material. Verify the persisted history for this fixed
		// template's two dimensions; PostAuthorized still checks the replay's
		// key/payload under the transaction's locks. This is deliberately a
		// conservative replay policy, not proof that the new payload matches.
		for _, entry := range authorized.Input.Entries {
			if _, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, entry.AccountHolder, entry.CurrencyUID, entry.ClassificationUID); err != nil {
				return nil, fmt.Errorf("verify replay history: %w", err)
			}
		}
	} else if err := core.VerifyJournalAuth(ctx, svc.AuthVerifier(), authorized.Input, authorized.EffectiveAt,
		authorized.Digest, authorized.Signature, authorized.KeyID); err != nil {
		return nil, fmt.Errorf("verify charge authorization: %w", err)
	}

	var journal *core.Journal
	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		// Acquire all journal/key and balance locks in the library's order,
		// before PostAuthorized or settlement takes any of its own locks.
		if err := tx.LockForTemplates(ctx, []core.TemplateExecutionRequest{request}, settleKey); err != nil {
			return err
		}
		var err error
		journal, err = tx.JournalWriter().PostAuthorized(ctx, authorized)
		if err != nil {
			return err
		}
		// The SAME amount feeds the charge and discharge. Returning every error
		// is essential: RunInTx only rolls back when its callback returns an error.
		// Discharge is unsigned in tx mode; see README's verified-hold boundary.
		if event.Partial {
			return tx.Reserver().SettlePartial(ctx, core.SettlePartialInput{
				ReservationUID: reservation.UID, Amount: event.Amount, IdempotencyKey: settleKey,
			})
		}
		return tx.Reserver().Settle(ctx, core.SettleInput{
			ReservationUID: reservation.UID, Amount: event.Amount, IdempotencyKey: settleKey,
		})
	})
	if err != nil {
		return nil, fmt.Errorf("post charge and settle: %w", err)
	}
	return journal, nil // publish a receipt only after the transaction commits
}
