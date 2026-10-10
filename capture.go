package ledger

import (
	"context"
	"fmt"
	"maps"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

const (
	captureReservationMetadataKey = "reservation_uid"
	captureModeMetadataKey        = "capture_mode"
	captureTemplateMetadataKey    = "capture_template_code"
)

// CaptureInput binds a positive charge to an existing reservation. Holder and
// currency come from that reservation, never from a separate caller argument.
type CaptureInput struct {
	ReservationUID string          `json:"reservation_uid"`
	Amount         decimal.Decimal `json:"amount"`
	IdempotencyKey string          `json:"idempotency_key"`
	// TemplateCode must consume exactly Amount of the holder's available
	// balance, with no entries for other user roles (including memo), users
	// or currencies. Its only amount parameter is "amount".
	TemplateCode string `json:"template_code"`
	// Partial records an incremental SettlePartial. False performs a one-shot
	// Settle and releases the unused reservation. FinalizeSettlement completes
	// a partially captured reservation; a zero-use operation uses Release.
	Partial bool   `json:"partial"`
	ActorID int64  `json:"actor_id"`
	Source  string `json:"source"`
	// Metadata is copied onto the charge. reservation_uid, capture_mode and
	// capture_template_code are reserved; supplying any of them is invalid.
	Metadata map[string]string `json:"metadata"`
}

// CaptureResult identifies the reservation and its charge journal. Replay
// returns the same values without applying another settlement or charge.
type CaptureResult struct {
	ReservationUID string `json:"reservation_uid"`
	JournalUID     string `json:"journal_uid"`
}

func (in CaptureInput) validate() error {
	if err := (core.SettleInput{ReservationUID: in.ReservationUID, Amount: in.Amount, IdempotencyKey: in.IdempotencyKey}).Validate(); err != nil {
		return fmt.Errorf("ledger: capture: %w", err)
	}
	if in.TemplateCode == "" {
		return fmt.Errorf("ledger: capture: template_code required: %w", core.ErrInvalidInput)
	}
	for _, key := range []string{captureReservationMetadataKey, captureModeMetadataKey, captureTemplateMetadataKey} {
		if _, exists := in.Metadata[key]; exists {
			return fmt.Errorf("ledger: capture: metadata[%q] is reserved: %w", key, core.ErrInvalidInput)
		}
	}
	return nil
}

// Capture atomically settles a reservation and posts its charge journal. It
// pre-acquires the complete sorted advisory-lock set before settlement takes
// the reservation row lock. The initial nonlocking reservation read supplies
// only its immutable holder/currency; Settle/SettlePartial recheck live state,
// expiry, precision and the remaining reserved amount under that row lock.
//
// The caller's key derives ":settle" and ":charge" keys. Keep the whole input
// unchanged on retries; a conflicting journal payload returns ErrConflict.
// Changing Partial can instead return ErrInvalidTransition from settlement,
// before journal replay runs. Both refuse the changed operation atomically.
// The result's JournalUID and the journal's reserved metadata link the charge;
// this method does not change the reservation's JournalUID field.
//
// On a top-level Service this opens RunInTx. On its callback's clone, Capture
// joins that transaction without a savepoint: the caller MUST return any error
// so partial writes roll back, and must pre-acquire the union of all locks when
// composing additional money operations. Ordinary journals and reservation
// discharge claims in this transaction are unsigned, even WithAttestor. For
// verifiable journals, compose AuthorizeTemplate outside the transaction and
// PostAuthorized inside it; unsigned discharge retains the verified-balance
// gate's conservative hold until expiry. Capture makes no external calls.
//
// A reservation protects against other reservations, not raw journal writes.
// Hosts must route consumption through the reservation flow and configure
// account policies; Capture cannot retroactively protect a hold already spent
// by a direct journal. Templates with memo or other non-available user entries
// require explicit host composition rather than this narrower convenience API.
func (s *Service) Capture(ctx context.Context, in CaptureInput) (*CaptureResult, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	metadata := maps.Clone(in.Metadata)
	if metadata == nil {
		metadata = make(map[string]string, 3)
	}
	metadata[captureReservationMetadataKey] = in.ReservationUID
	metadata[captureModeMetadataKey] = "full"
	if in.Partial {
		metadata[captureModeMetadataKey] = "partial"
	}
	metadata[captureTemplateMetadataKey] = in.TemplateCode
	result := &CaptureResult{ReservationUID: in.ReservationUID}
	body := func(tx *Service) error {
		r, err := tx.ReservationReader().GetReservation(ctx, in.ReservationUID)
		if err != nil {
			return fmt.Errorf("ledger: capture: read reservation: %w", err)
		}
		request := core.TemplateExecutionRequest{TemplateCode: in.TemplateCode, Params: core.TemplateParams{
			HolderID: r.AccountHolder, CurrencyUID: r.CurrencyUID, IdempotencyKey: in.IdempotencyKey + ":charge",
			Amounts: map[string]decimal.Decimal{"amount": in.Amount}, ActorID: in.ActorID, Source: in.Source, Metadata: metadata,
		}}
		if err := tx.LockForTemplates(ctx, []core.TemplateExecutionRequest{request}, in.IdempotencyKey+":settle"); err != nil {
			return fmt.Errorf("ledger: capture: lock: %w", err)
		}
		if in.Partial {
			err = tx.Reserver().SettlePartial(ctx, core.SettlePartialInput{ReservationUID: r.UID, Amount: in.Amount, IdempotencyKey: in.IdempotencyKey + ":settle"})
		} else {
			err = tx.Reserver().Settle(ctx, core.SettleInput{ReservationUID: r.UID, Amount: in.Amount, IdempotencyKey: in.IdempotencyKey + ":settle"})
		}
		if err != nil {
			return fmt.Errorf("ledger: capture: settle: %w", err)
		}
		journal, err := tx.JournalWriter().ExecuteTemplate(ctx, request.TemplateCode, request.Params)
		if err != nil {
			return fmt.Errorf("ledger: capture: charge: %w", err)
		}
		_, entries, err := tx.Queries().GetJournal(ctx, journal.UID)
		if err != nil {
			return fmt.Errorf("ledger: capture: read charge: %w", err)
		}
		classes, err := classificationRoles(ctx, tx)
		if err != nil {
			return err
		}
		if err := validateCaptureEntries(entries, r.AccountHolder, r.CurrencyUID, in.Amount, classes); err != nil {
			return err
		}
		result.JournalUID = journal.UID
		return nil
	}
	if s.tx != nil {
		if err := body(s); err != nil {
			return nil, err
		}
	} else if err := s.RunInTx(ctx, body); err != nil {
		return nil, err
	}
	return result, nil
}

func validateCaptureEntries(entries []core.Entry, holder int64, currency string, amount decimal.Decimal, classes map[string]core.Classification) error {
	net := decimal.Zero
	for _, entry := range entries {
		if entry.CurrencyUID != currency || (entry.AccountHolder != holder && entry.AccountHolder != core.SystemAccountHolder(holder)) {
			return fmt.Errorf("ledger: capture: charge touched another holder or currency: %w", core.ErrInvalidInput)
		}
		if entry.AccountHolder != holder {
			continue
		}
		class, ok := classes[entry.ClassificationUID]
		if !ok || class.BalanceRole != core.BalanceRoleAvailable {
			return fmt.Errorf("ledger: capture: charge must touch only the holder's available classifications: %w", core.ErrInvalidInput)
		}
		signed, err := core.SignedAmount(class.NormalSide, entry.EntryType, entry.Amount)
		if err != nil {
			return fmt.Errorf("ledger: capture: charge direction: %w", err)
		}
		net = net.Add(signed)
	}
	if !net.Equal(amount.Neg()) {
		return fmt.Errorf("ledger: capture: charge must decrease available by exactly %s: %w", amount, core.ErrInvalidInput)
	}
	return nil
}
