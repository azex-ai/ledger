// Example: deposit 1 USDC, buy 1,000 AI credits, and charge usage.
//
// Uses configured fixed rates and existing ledger primitives; usage events belong to the host.
// This example has no withdrawal or credit cash-out path. Run against a dedicated
// example database; fixed operation keys replay completed operations without
// duplicate accounting. Interrupted jobs whose holds expire need reconciliation.
//
// Run with DATABASE_URL (runtime) and MIGRATE_DATABASE_URL (migration credential):
//
//	go run ./examples/credits-topup
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"maps"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

const userID int64 = 2001

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	data := defaultRates
	if path := os.Getenv("CREDITS_RATES_FILE"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read rates configuration: %w", err)
		}
	}
	prices, err := parsePricing(data)
	if err != nil {
		return err
	}
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	migrateURL := os.Getenv("MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		return fmt.Errorf("MIGRATE_DATABASE_URL is required; use a separate migration credential")
	}
	if err := ledger.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		return err
	}
	if err := svc.AssertRuntimeRole(ctx); err != nil {
		return err
	}
	usdc, credits, err := setup(ctx, svc, prices)
	if err != nil {
		return err
	}
	if err := scenario(ctx, svc, usdc, credits, prices); err != nil {
		return err
	}
	fmt.Println("Configured USDC → CREDITS purchase and usage completed; balances verified, no outstanding holds.")
	fmt.Println("Replaying the same events with their original rate configuration is a no-op.")
	return nil
}

func setup(ctx context.Context, svc *ledger.Service, prices pricing) (string, string, error) {
	source, sourceOK := prices.units["USDC"]
	target, targetOK := prices.units["CREDITS"]
	if !sourceOK || !targetOK {
		return "", "", fmt.Errorf("example requires USDC and CREDITS units: %w", core.ErrInvalidInput)
	}
	for _, bundle := range []presets.TemplateBundle{presets.DepositBundle(), presets.FXBundle()} {
		if err := presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), bundle); err != nil {
			return "", "", err
		}
	}
	usdc, err := ensureCurrency(ctx, svc, source.Code, source.Name, source.Exponent)
	if err != nil {
		return "", "", err
	}
	// Wallet precision is configured once when the currency is created.
	credits, err := ensureCurrency(ctx, svc, target.Code, target.Name, target.Exponent)
	if err != nil {
		return "", "", err
	}
	if err := ensureSpendTemplate(ctx, svc); err != nil {
		return "", "", err
	}
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return "", "", err
	}
	for _, currency := range []string{usdc, credits} {
		if _, err := svc.AccountPolicies().SetPolicy(ctx, core.AccountPolicyInput{
			AccountHolder: userID, CurrencyUID: currency, ClassificationUID: wallet.UID,
			Status: core.AccountPolicyStatusActive, EnforceMinBalance: true,
			MinBalance: decimal.Zero, Note: "Prepaid credits example: no overdraft",
		}); err != nil {
			return "", "", err
		}
	}
	return usdc, credits, nil
}

// scenario represents already-confirmed deposit and usage events. Production
// hosts persist their event/request IDs and reuse them on delivery retries.
func scenario(ctx context.Context, svc *ledger.Service, usdc, credits string, prices pricing) error {
	const root = "credits-demo-v2"
	target, err := svc.Currencies().GetCurrency(ctx, credits)
	if err != nil {
		return err
	}
	// Price measured quantities before any bookkeeping. Each source unit uses
	// its configured directed rate and the wallet's actual target precision.
	quote := func(unit string, quantity int64) (pricedAmount, error) {
		return prices.price(ctx, *target, usageQuantity{unit, decimal.NewFromInt(quantity)})
	}
	purchase, err := quote("USDC", 1)
	if err != nil {
		return err
	}
	// The purchase rate is resolved here, through the RateQuoter port, before
	// any transaction opens. Exchange receives the value, never the port.
	purchaseRate, err := prices.QuoteRate(ctx, "USDC", "CREDITS")
	if err != nil {
		return err
	}
	image, err := quote("IMAGE", 1)
	if err != nil {
		return err
	}
	metered, err := prices.price(ctx, *target,
		usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(10000)},
		usageQuantity{"OUTPUT_TOKEN", decimal.NewFromInt(2425)})
	if err != nil {
		return err
	}
	meterBudget, err := prices.price(ctx, *target,
		usageQuantity{"INPUT_TOKEN", decimal.NewFromInt(15000)},
		usageQuantity{"OUTPUT_TOKEN", decimal.NewFromInt(4000)})
	if err != nil {
		return err
	}
	free, err := quote("IMAGE", 0)
	if err != nil {
		return err
	}
	freeBudget, err := quote("IMAGE", 2)
	if err != nil {
		return err
	}
	streamBudget, err := quote("STREAM_TOKEN", 10000)
	if err != nil {
		return err
	}
	first, err := quote("STREAM_TOKEN", 1000)
	if err != nil {
		return err
	}
	second, err := quote("STREAM_TOKEN", 2000)
	if err != nil {
		return err
	}
	// The demo requires a positive purchase, each budget and each stream delta.
	// Reject a zero-rounded configuration before even the deposit fixture writes.
	for _, amount := range []decimal.Decimal{purchase.amount, image.amount, meterBudget.amount, freeBudget.amount, streamBudget.amount, first.amount, second.amount} {
		if !amount.IsPositive() {
			return fmt.Errorf("example purchase, budget or stream delta rounds to zero: %w", core.ErrInvalidInput)
		}
	}

	// This is a local confirmed-deposit fixture. Production hosts accept a
	// trusted chain confirmation, never an amount asserted by a browser.
	depositJournal, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: userID, CurrencyUID: usdc, IdempotencyKey: root + ":deposit",
		Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)}, Source: "credits-topup-example",
	})
	if err != nil {
		return err
	}
	// The library's Exchange reserves and settles the USDC, posts both FX
	// legs, and records the quote on each -- all in one transaction, with the
	// lock order a concurrent deposit also follows. FundingUID links the
	// purchase to the deposit that paid for it: a host reconciliation joins
	// confirmed deposits against exchanges carrying their uid to find one
	// that was confirmed and never converted. It must be a journal uid with
	// an entry for this holder in USDC -- Exchange refuses anything else. A
	// production host passes its confirmed deposit booking's JournalUID here.
	if _, err := svc.Exchange(ctx, ledger.ExchangeInput{
		HolderID: userID, SourceCurrencyUID: usdc, TargetCurrencyUID: credits,
		Quantity: decimal.NewFromInt(1), Rate: purchaseRate,
		IdempotencyKey: root + ":purchase", FundingUID: depositJournal.UID,
		Metadata: map[string]string{"purchase_id": root + ":purchase"},
		Source:   "credits-topup-example",
	}); err != nil {
		return err
	}

	// Provider work happens between Reserve and capture, outside transactions.
	// These quantity snapshots stand in for durable, immutable provider results.
	for _, job := range []struct {
		key    string
		budget decimal.Decimal
		actual pricedAmount
	}{
		{"image", image.amount, image},
		{"tokens", meterBudget.amount, metered},
		{"failed", freeBudget.amount, free},
	} {
		rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder: userID, CurrencyUID: credits, Amount: job.budget,
			ExpiresIn: time.Hour, IdempotencyKey: root + ":" + job.key + ":reserve",
		})
		if err != nil {
			return err
		}
		metadata, err := job.actual.metadata()
		if err != nil {
			return err
		}
		if err := captureUsage(ctx, svc, rsv, job.actual.amount, root+":"+job.key+":result", false, metadata); err != nil {
			return err
		}
	}

	// A stream prices durable deltas. This example rounds each delta at the
	// configured precision; cumulative provider counters must first be normalized.
	rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: userID, CurrencyUID: credits, Amount: streamBudget.amount,
		ExpiresIn: time.Hour, IdempotencyKey: root + ":stream:reserve",
	})
	if err != nil {
		return err
	}
	for i, usage := range []pricedAmount{first, second} {
		metadata, err := usage.metadata()
		if err != nil {
			return err
		}
		if err := captureUsage(ctx, svc, rsv, usage.amount, fmt.Sprintf("%s:stream:event-%d", root, i), true, metadata); err != nil {
			return err
		}
	}
	if err := svc.Reserver().FinalizeSettlement(ctx, core.FinalizeSettlementInput{
		ReservationUID: rsv.UID, IdempotencyKey: root + ":stream:finalize",
	}); err != nil {
		return err
	}
	expected := purchase.amount.Sub(image.amount).Sub(metered.amount).Sub(first.amount).Sub(second.amount)
	return checkFinalBalances(ctx, svc, usdc, credits, expected)
}

// captureUsage takes the trusted reservation returned by Reserve, never a
// browser-supplied holder/currency. All usage goes through this reservation flow;
// raw journals can bypass holds even with a min-balance policy.
// The host must persist an immutable event payload (amount and operation kind)
// before calling this helper. Ledger keys deduplicate individual operations,
// not a provider event changed from a charged delta into a zero-cost release.
// For services using WithAttestor, AuthorizeTemplate before RunInTx and then
// PostAuthorized inside it; see examples/tamper-evident for the signed variant.
func captureUsage(ctx context.Context, svc *ledger.Service, rsv *core.Reservation, amount decimal.Decimal, key string, partial bool, quoteMetadata map[string]string) error {
	if rsv == nil || key == "" || amount.IsNegative() {
		return core.ErrInvalidInput
	}
	if amount.IsZero() {
		if partial {
			return core.ErrInvalidInput
		} // final zero-use event releases, not a stream increment
		return svc.Reserver().Release(ctx, core.ReleaseInput{ReservationUID: rsv.UID, IdempotencyKey: key + ":release"})
	}
	metadata := maps.Clone(quoteMetadata)
	if metadata == nil {
		metadata = make(map[string]string)
	}
	metadata["usage_event_id"] = key
	metadata["reservation_uid"] = rsv.UID
	return svc.RunInTx(ctx, func(tx *ledger.Service) error {
		var err error
		if partial {
			err = tx.Reserver().SettlePartial(ctx, core.SettlePartialInput{ReservationUID: rsv.UID, Amount: amount, IdempotencyKey: key + ":settle"})
		} else {
			err = tx.Reserver().Settle(ctx, core.SettleInput{ReservationUID: rsv.UID, Amount: amount, IdempotencyKey: key + ":settle"})
		}
		if err != nil {
			return err
		}
		_, err = tx.JournalWriter().ExecuteTemplate(ctx, "credits_spend", core.TemplateParams{
			HolderID: rsv.AccountHolder, CurrencyUID: rsv.CurrencyUID, IdempotencyKey: key + ":charge",
			Amounts:  map[string]decimal.Decimal{"amount": amount},
			Metadata: metadata,
			Source:   "credits-topup-example",
		})
		return err
	})
}

func checkFinalBalances(ctx context.Context, svc *ledger.Service, usdc, credits string, expected decimal.Decimal) error {
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return err
	}
	for _, want := range []struct{ currency, balance string }{{usdc, "0"}, {credits, expected.String()}} {
		got, err := svc.BalanceReader().GetBalance(ctx, userID, want.currency, wallet.UID)
		if err != nil {
			return err
		}
		if !got.Equal(decimal.RequireFromString(want.balance)) {
			return fmt.Errorf("balance %s: got %s, want %s", want.currency, got, want.balance)
		}
		held, err := svc.Reserver().HeldAmount(ctx, userID, want.currency)
		if err != nil {
			return err
		}
		if !held.IsZero() {
			return fmt.Errorf("outstanding hold: %s", held)
		}
	}
	return nil
}

func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string, exponent int32) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	for _, c := range list {
		if c.Code != code {
			continue
		}
		if c.Exponent != exponent {
			return "", fmt.Errorf("currency %s already exists with exponent %d, this example expects %d", code, c.Exponent, exponent)
		}
		return c.UID, nil
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: exponent})
	if err != nil {
		return "", fmt.Errorf("create currency %s: %w", code, err)
	}
	return created.UID, nil
}

// ensureSpendTemplate registers credits_spend (credits leave the wallet back to
// settlement, reducing outstanding credit liability).
func ensureSpendTemplate(ctx context.Context, svc *ledger.Service) error {
	if _, err := svc.Templates().GetTemplate(ctx, "credits_spend"); err == nil {
		return nil
	} else if !errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("get template credits_spend: %w", err)
	}
	jt, err := ensureJournalType(ctx, svc, "credits_spend", "Credits Spend")
	if err != nil {
		return err
	}
	mw, st, _, err := classIDs(ctx, svc, "main_wallet", "settlement", "settlement")
	if err != nil {
		return err
	}
	_, err = svc.Templates().CreateTemplate(ctx, core.TemplateInput{
		Code: "credits_spend", Name: "Credits Spend", JournalTypeUID: jt,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: st, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: mw, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	})
	if err != nil {
		return fmt.Errorf("create credits_spend template: %w", err)
	}
	return nil
}

func ensureJournalType(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	existing, err := svc.JournalTypes().GetJournalTypeByCode(ctx, code)
	if err == nil {
		return existing.UID, nil
	}
	if !errors.Is(err, core.ErrNotFound) {
		return "", fmt.Errorf("get journal type %s: %w", code, err)
	}
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name, DisplayLabel: "AI usage", HolderKind: core.HolderTxKindFee})
	if err != nil {
		return "", fmt.Errorf("create journal type %s: %w", code, err)
	}
	return jt.UID, nil
}

func classIDs(ctx context.Context, svc *ledger.Service, a, b, c string) (string, string, string, error) {
	ca, err := svc.Classifications().GetByCode(ctx, a)
	if err != nil {
		return "", "", "", fmt.Errorf("get %s: %w", a, err)
	}
	cb, err := svc.Classifications().GetByCode(ctx, b)
	if err != nil {
		return "", "", "", fmt.Errorf("get %s: %w", b, err)
	}
	cc, err := svc.Classifications().GetByCode(ctx, c)
	if err != nil {
		return "", "", "", fmt.Errorf("get %s: %w", c, err)
	}
	return ca.UID, cb.UID, cc.UID, nil
}
