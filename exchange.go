package ledger

import (
	"context"
	"fmt"
	"maps"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
)

// exchangeReserveTTL bounds the hold Exchange takes on the source balance.
// The hold is settled inside the same transaction, so the TTL never elapses
// in practice; it exists because Reserve requires a positive expiry.
const exchangeReserveTTL = time.Minute

// ExchangeInput describes one conversion between two stored wallet balances
// of the same holder: Quantity of the source currency leaves, the quoted
// amount of the target currency arrives.
type ExchangeInput struct {
	// HolderID is the user whose balances move. Positive (user side).
	HolderID int64
	// SourceCurrencyUID / TargetCurrencyUID are the two stored currencies.
	// Both must exist; Rate's codes must match theirs.
	SourceCurrencyUID string
	TargetCurrencyUID string
	// Quantity is the source amount, at the source currency's precision.
	Quantity decimal.Decimal
	// Rate is the quote resolved OUTSIDE the transaction, typically from a
	// core.RateQuoter. Exchange never resolves rates itself.
	Rate core.FixedRate
	// IdempotencyKey identifies this exchange. The four ledger operations
	// derive their own keys from it (":reserve", ":settle", ":pay",
	// ":issue"); replay with the same key and the same Rate/Quantity is a
	// no-op, the same key with a different quote is core.ErrConflict.
	IdempotencyKey string
	// FundingUID, when set, is recorded on both journals under
	// core.FundingUIDMetadataKey: the uid of the confirmed deposit booking
	// (or deposit journal) this exchange spends. Optional to the ledger,
	// but a host that converts deposits into credits should always set it --
	// it is what its reconciliation joins on to find a deposit that was
	// confirmed and never converted.
	FundingUID string
	// Metadata is copied onto both journals. It must not use the keys the
	// ledger writes itself (core.ConversionQuotesMetadataKey,
	// core.FundingUIDMetadataKey); doing so is core.ErrInvalidInput.
	Metadata map[string]string
	// ActorID and Source are passed through to both journals.
	ActorID int64
	Source  string
	// SellTemplateCode / BuyTemplateCode override the FX preset templates
	// (presets.FXBundle's "fx_sell" / "fx_buy"). Each must take one "amount"
	// and move the user's main wallet against a system counterpart in the
	// currency it is executed with.
	SellTemplateCode string
	BuyTemplateCode  string
}

// ExchangeResult reports what one Exchange call did or, on replay, what the
// original call did.
type ExchangeResult struct {
	// Quote is the complete snapshot applied: units, precision, quantity,
	// rate, rounding, resulting amount. The same value is stored on both
	// journals under core.ConversionQuotesMetadataKey.
	Quote core.ConversionQuote
	// ReservationUID is the settled hold on the source balance.
	ReservationUID string
	// SellJournalUID / BuyJournalUID are the two FX legs.
	SellJournalUID string
	BuyJournalUID  string
}

func (in ExchangeInput) validate() error {
	switch {
	case in.HolderID <= 0:
		return fmt.Errorf("ledger: exchange: holder_id must be positive: %w", core.ErrInvalidInput)
	case in.IdempotencyKey == "":
		return fmt.Errorf("ledger: exchange: idempotency key required: %w", core.ErrInvalidInput)
	case in.SourceCurrencyUID == "" || in.TargetCurrencyUID == "":
		return fmt.Errorf("ledger: exchange: source and target currency required: %w", core.ErrInvalidInput)
	case in.SourceCurrencyUID == in.TargetCurrencyUID:
		return fmt.Errorf("ledger: exchange: source and target currency must differ: %w", core.ErrInvalidInput)
	case !in.Quantity.IsPositive():
		return fmt.Errorf("ledger: exchange: quantity must be positive: %w", core.ErrInvalidInput)
	}
	for _, reserved := range []string{core.ConversionQuotesMetadataKey, core.FundingUIDMetadataKey} {
		if _, taken := in.Metadata[reserved]; taken {
			return fmt.Errorf("ledger: exchange: metadata[%q] is written by the ledger: %w", reserved, core.ErrInvalidInput)
		}
	}
	return nil
}

// Exchange converts Quantity of the holder's source currency into the target
// currency at the supplied quote, atomically: it reserves and settles the
// source amount (so account policies and balance floors apply exactly as they
// do to any other spend -- a raw journal would bypass a hold), then posts the
// sell and buy FX legs. All four writes commit or roll back together, and
// both journals carry the applied core.ConversionQuote and, when given, the
// funding reference.
//
// This is the composition docs/COOKBOOK.md Recipe 1 describes and
// examples/credits-topup used to hand-roll. It exists in the library because
// the lock order is the part a host gets wrong: Reserve alone locks the
// source pair first, the FX legs also need the system and target pairs, and a
// concurrent deposit acquires the same set in another order. Exchange takes
// the union up front via LockForTemplates, so it orders identically to any
// other template write.
//
// Call it on the top-level Service and it opens its own RunInTx; call it on
// the *Service a RunInTx callback receives and it joins that transaction, so
// a host can mark its own deposit row converted in the same commit. Either
// way the Rate must already be resolved: nothing here calls a RateQuoter, and
// nothing may (financial.md: no external call inside a transaction).
//
// When it joins a caller's transaction there is no savepoint: a failed
// Exchange does not undo what it already wrote inside that transaction (the
// reservation, the sell leg, ...). The host must return Exchange's error from
// the RunInTx callback so the whole transaction rolls back; a callback that
// swallows the error and returns nil commits the partial write.
//
// A quote whose output rounds to zero is refused before any write. Journals
// posted through the transaction path are unsigned (core.AuthStatusUnsignedTxMode,
// see RunInTx); a WithAttestor deployment that needs verifiable FX legs
// composes AuthorizeTemplate + PostAuthorized by hand, as RunInTx's doc
// comment describes.
//
// Retry by replaying the call with the same IdempotencyKey, Quantity and
// Rate: every operation replays to its original result. The same key with a
// different quote, quantity or funding reference is core.ErrConflict, even
// when the different quote rounds to the same amount, because the quote is
// part of each journal's compared payload.
func (s *Service) Exchange(ctx context.Context, in ExchangeInput) (*ExchangeResult, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	sellTemplate, buyTemplate := in.SellTemplateCode, in.BuyTemplateCode
	if sellTemplate == "" {
		sellTemplate = "fx_sell"
	}
	if buyTemplate == "" {
		buyTemplate = "fx_buy"
	}

	source, err := s.currencyStore.GetCurrency(ctx, in.SourceCurrencyUID)
	if err != nil {
		return nil, fmt.Errorf("ledger: exchange: source currency: %w", err)
	}
	target, err := s.currencyStore.GetCurrency(ctx, in.TargetCurrencyUID)
	if err != nil {
		return nil, fmt.Errorf("ledger: exchange: target currency: %w", err)
	}
	quote, err := in.Rate.Quote(in.Quantity, *source, *target)
	if err != nil {
		return nil, fmt.Errorf("ledger: exchange: %w", err)
	}
	if !quote.TargetAmount.IsPositive() {
		return nil, fmt.Errorf("ledger: exchange: %s %s converts to zero %s at the quoted rate: %w",
			in.Quantity, source.Code, target.Code, core.ErrInvalidInput)
	}
	encodedQuote, err := core.EncodeConversionQuotes([]core.ConversionQuote{quote})
	if err != nil {
		return nil, fmt.Errorf("ledger: exchange: %w", err)
	}
	metadata := maps.Clone(in.Metadata)
	if metadata == nil {
		metadata = make(map[string]string, 2)
	}
	metadata[core.ConversionQuotesMetadataKey] = encodedQuote
	if in.FundingUID != "" {
		metadata[core.FundingUIDMetadataKey] = in.FundingUID
	}

	key := in.IdempotencyKey
	requests := []core.TemplateExecutionRequest{
		{TemplateCode: sellTemplate, Params: core.TemplateParams{
			HolderID: in.HolderID, CurrencyUID: in.SourceCurrencyUID, IdempotencyKey: key + ":pay",
			Amounts: map[string]decimal.Decimal{"amount": in.Quantity},
			ActorID: in.ActorID, Source: in.Source, Metadata: metadata,
		}},
		{TemplateCode: buyTemplate, Params: core.TemplateParams{
			HolderID: in.HolderID, CurrencyUID: in.TargetCurrencyUID, IdempotencyKey: key + ":issue",
			Amounts: map[string]decimal.Decimal{"amount": quote.TargetAmount},
			ActorID: in.ActorID, Source: in.Source, Metadata: metadata,
		}},
	}

	result := &ExchangeResult{Quote: quote}
	body := func(tx *Service) error {
		// The union of every lock this transaction will take, in the one
		// canonical order -- before Reserve takes the source pair on its own.
		// Every idempotency key the transaction later uses is named here:
		// the two template keys ride in `requests`, Reserve's and Settle's
		// are the extras. Settle does not take an advisory idempotency lock
		// today (its replay guard is the reservation row lock), but leaving
		// its key out of the set would make the pre-acquired order silently
		// incomplete the day it does -- the ABBA shape LockForTemplates
		// exists to rule out.
		if err := tx.LockForTemplates(ctx, requests, key+":reserve", key+":settle"); err != nil {
			return fmt.Errorf("ledger: exchange: %w", err)
		}
		rsv, err := tx.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder: in.HolderID, CurrencyUID: in.SourceCurrencyUID, Amount: in.Quantity,
			ExpiresIn: exchangeReserveTTL, IdempotencyKey: key + ":reserve",
		})
		if err != nil {
			return fmt.Errorf("ledger: exchange: reserve: %w", err)
		}
		result.ReservationUID = rsv.UID
		if err := tx.Reserver().Settle(ctx, core.SettleInput{
			ReservationUID: rsv.UID, Amount: in.Quantity, IdempotencyKey: key + ":settle",
		}); err != nil {
			return fmt.Errorf("ledger: exchange: settle: %w", err)
		}
		journals, err := tx.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, requests)
		if err != nil {
			return fmt.Errorf("ledger: exchange: fx legs: %w", err)
		}
		if len(journals) != 2 {
			// Cannot happen while ExecuteTemplateBatch returns one journal per
			// request; failing loudly beats indexing into a shorter slice.
			return fmt.Errorf("ledger: exchange: fx legs: expected 2 journals, got %d", len(journals))
		}
		result.SellJournalUID, result.BuyJournalUID = journals[0].UID, journals[1].UID
		return nil
	}

	if s.tx != nil {
		// Already inside the caller's RunInTx: join it.
		if err := body(s); err != nil {
			return nil, err
		}
		return result, nil
	}
	if err := s.RunInTx(ctx, body); err != nil {
		return nil, err
	}
	return result, nil
}
