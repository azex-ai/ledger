// Example: deposit 1 USDC, buy 1,000 AI credits, and charge usage.
//
// Uses existing ledger primitives; pricing and usage policy belong to the host.
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
	usdc, credits, err := setup(ctx, svc)
	if err != nil {
		return err
	}
	if err := scenario(ctx, svc, usdc, credits); err != nil {
		return err
	}
	fmt.Println("1 USDC → 1,000 credits; fixed 25 + metered 32.125 + streamed 30 = 87.125 credits spent")
	fmt.Println("Remaining: 912.875 credits; 0 USDC; no outstanding holds. Re-running is a no-op.")
	return nil
}

func setup(ctx context.Context, svc *ledger.Service) (string, string, error) {
	for _, bundle := range []presets.TemplateBundle{presets.DepositBundle(), presets.FXBundle()} {
		if err := presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), bundle); err != nil {
			return "", "", err
		}
	}
	usdc, err := ensureCurrency(ctx, svc, "USDC", "USD Coin", 6)
	if err != nil {
		return "", "", err
	}
	// Six fractional credit places are an explicit example policy, allowing
	// token-metered prices. Hosts choose their own precision and rounding policy.
	credits, err := ensureCurrency(ctx, svc, "CREDITS", "AI Credits", 6)
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
func scenario(ctx context.Context, svc *ledger.Service, usdc, credits string) error {
	const root = "credits-demo-v2"
	// Local fixture only: in production the crypto-deposit adapter confirms the
	// actual onchain receipt before this balance can fund a purchase. Never call
	// deposit_confirm based on an amount asserted by a browser.
	if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: userID, CurrencyUID: usdc, IdempotencyKey: root + ":deposit",
		Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)}, Source: "credits-topup-example",
	}); err != nil {
		return err
	}
	if err := purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), root+":purchase"); err != nil {
		return err
	}

	// External provider calls belong BETWEEN Reserve and capture, outside DB
	// transactions. These completed events stand in for durable provider results.
	for _, job := range []struct{ key, budget, actual string }{
		{"image", "25", "25"},      // fixed price per completed image
		{"tokens", "50", "32.125"}, // input/output/cached-token usage priced by the host
		{"failed", "40", "0"},      // failure before billable work, or free/cache hit
	} {
		rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder: userID, CurrencyUID: credits,
			Amount: decimal.RequireFromString(job.budget), ExpiresIn: time.Hour,
			IdempotencyKey: root + ":" + job.key + ":reserve",
		})
		if err != nil {
			return err
		}
		if err := captureCredits(ctx, svc, rsv, decimal.RequireFromString(job.actual), root+":"+job.key+":result", false); err != nil {
			return err
		}
	}

	// Streaming/multi-step agent: charge INCREMENTS with stable event IDs. A
	// repeated provider event is a no-op; a cumulative counter must first be
	// converted to a delta by the host's durable usage processor.
	rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: userID, CurrencyUID: credits, Amount: decimal.NewFromInt(100),
		ExpiresIn: time.Hour, IdempotencyKey: root + ":stream:reserve",
	})
	if err != nil {
		return err
	}
	for i, amount := range []int64{10, 20} {
		if err := captureCredits(ctx, svc, rsv, decimal.NewFromInt(amount), fmt.Sprintf("%s:stream:event-%d", root, i), true); err != nil {
			return err
		}
	}
	if err := svc.Reserver().FinalizeSettlement(ctx, core.FinalizeSettlementInput{
		ReservationUID: rsv.UID, IdempotencyKey: root + ":stream:finalize",
	}); err != nil {
		return err
	}
	return checkFinalBalances(ctx, svc, usdc, credits)
}

// purchaseCredits is host composition, not a new library billing subsystem.
// The host fixes the quote at 1 USDC : 1,000 credits. A reservation keeps other
// compliant purchases from consuming the same USDC. The hold and both currency
// journals share a transaction, including when a later leg fails.
func purchaseCredits(ctx context.Context, svc *ledger.Service, usdc, credits string, amount decimal.Decimal, key string) error {
	if key == "" || usdc == credits {
		return core.ErrInvalidInput
	}
	return svc.RunInTx(ctx, func(tx *ledger.Service) error {
		rsv, err := tx.Reserver().Reserve(ctx, core.ReserveInput{
			AccountHolder: userID, CurrencyUID: usdc, Amount: amount,
			ExpiresIn: time.Minute, IdempotencyKey: key + ":reserve",
		})
		if err != nil {
			return err
		}
		if err := tx.Reserver().Settle(ctx, core.SettleInput{
			ReservationUID: rsv.UID, Amount: amount, IdempotencyKey: key + ":settle",
		}); err != nil {
			return err
		}
		meta := map[string]string{"purchase_id": key, "pricing_version": "demo-v1", "credits_per_usdc": "1000"}
		_, err = tx.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
			{TemplateCode: "fx_sell", Params: core.TemplateParams{
				HolderID: userID, CurrencyUID: usdc, IdempotencyKey: key + ":pay",
				Amounts: map[string]decimal.Decimal{"amount": amount}, Metadata: meta,
			}},
			{TemplateCode: "fx_buy", Params: core.TemplateParams{
				HolderID: userID, CurrencyUID: credits, IdempotencyKey: key + ":issue",
				Amounts: map[string]decimal.Decimal{"amount": amount.Mul(decimal.NewFromInt(1000))}, Metadata: meta,
			}},
		})
		return err
	})
}

// captureCredits takes the trusted reservation returned by Reserve, never a
// browser-supplied holder/currency. All usage goes through this reservation flow;
// raw journals can bypass holds even with a min-balance policy.
// For services using WithAttestor, AuthorizeTemplate before RunInTx and then
// PostAuthorized inside it; see examples/tamper-evident for the signed variant.
func captureCredits(ctx context.Context, svc *ledger.Service, rsv *core.Reservation, amount decimal.Decimal, key string, partial bool) error {
	if rsv == nil || key == "" || amount.IsNegative() {
		return core.ErrInvalidInput
	}
	if amount.IsZero() {
		if partial {
			return core.ErrInvalidInput
		} // final zero-use event releases, not a stream increment
		return svc.Reserver().Release(ctx, core.ReleaseInput{ReservationUID: rsv.UID, IdempotencyKey: key + ":release"})
	}
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
			Metadata: map[string]string{"usage_event_id": key, "reservation_uid": rsv.UID, "pricing_version": "demo-v1"},
			Source:   "credits-topup-example",
		})
		return err
	})
}

func checkFinalBalances(ctx context.Context, svc *ledger.Service, usdc, credits string) error {
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return err
	}
	for _, want := range []struct{ currency, balance string }{{usdc, "0"}, {credits, "912.875"}} {
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
