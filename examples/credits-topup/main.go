// Example: buying, bonusing, spending, and cashing out credits.
//
// Walks the Ledger Cookbook (docs/COOKBOOK.md) end-to-end:
//   - Recipe 1  buy credits at 1 USDT : 100 credits (FX two-leg, atomic batch)
//   - Recipe 2b top-up with bonus (runtime-registered template, equity-funded)
//   - Recipe 4  spend credits via Reserve → Settle → actual-debit journal
//   - Recipe 5  cash credits back to USDT (reverse FX two-leg)
//
// Demonstrates that credits are "just another currency": the same main_wallet /
// settlement / equity classifications and fx_sell / fx_buy templates work across
// USDT and credits without any credit-specific code.
//
// Run:
//
//	export DATABASE_URL="postgres://user:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/credits-topup
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres"
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
	// postgres.Migrate uses golang-migrate's pgx5 driver (pgx5:// scheme);
	// pgxpool.New wants postgres://. Accept a standard postgres:// URL and
	// translate for the migrator.
	migrateURL := dbURL
	if rest, ok := strings.CutPrefix(dbURL, "postgres://"); ok {
		migrateURL = "pgx5://" + rest
	}
	if err := postgres.Migrate(migrateURL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("pgxpool: %w", err)
	}
	defer pool.Close()

	svc, err := ledger.New(pool)
	if err != nil {
		return fmt.Errorf("ledger.New: %w", err)
	}
	if err := svc.InstallExtendedPresets(ctx); err != nil {
		return fmt.Errorf("install presets: %w", err)
	}

	// Two currencies — credits is just another row in `currencies`.
	usdtID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}
	creditsID, err := ensureCurrency(ctx, svc, "CREDITS", "Platform Credits")
	if err != nil {
		return err
	}

	mainWallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return fmt.Errorf("get main_wallet: %w", err)
	}
	creditsBal := func() (decimal.Decimal, error) {
		return svc.BalanceReader().GetBalance(ctx, userID, creditsID, mainWallet.ID)
	}
	usdtBal := func() (decimal.Decimal, error) {
		return svc.BalanceReader().GetBalance(ctx, userID, usdtID, mainWallet.ID)
	}

	// -----------------------------------------------------------------------
	// Seed: give the user 10 USDT to spend (a confirmed deposit).
	// -----------------------------------------------------------------------
	if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: userID, CurrencyID: usdtID, IdempotencyKey: ledger.NewIdempotencyKey("seed-usdt"),
		Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("10")},
		Source:  "credits-topup-example",
	}); err != nil {
		return fmt.Errorf("seed usdt: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after seeding 10 USDT")

	// -----------------------------------------------------------------------
	// Recipe 1: buy 100 credits for 1 USDT (FX two-leg, posted atomically).
	// -----------------------------------------------------------------------
	buyKey := ledger.NewIdempotencyKey("buy-credits")
	buyMeta := map[string]string{"quote_id": "q-1", "fx_rate": "100"}
	if _, err := svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
		{TemplateCode: "fx_sell", Params: core.TemplateParams{
			HolderID: userID, CurrencyID: usdtID, IdempotencyKey: buyKey + "-sell",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("1")}, Metadata: buyMeta,
		}},
		{TemplateCode: "fx_buy", Params: core.TemplateParams{
			HolderID: userID, CurrencyID: creditsID, IdempotencyKey: buyKey + "-buy",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("100")}, Metadata: buyMeta,
		}},
	}); err != nil {
		return fmt.Errorf("recipe 1 buy credits: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after buying 100 credits for 1 USDT")

	// -----------------------------------------------------------------------
	// Recipe 2b: top up with a bonus — pay 1 USDT, get 100 + 20 free credits.
	// The bonus is platform-funded out of equity via a runtime-registered
	// template with two amount keys (purchased + bonus).
	// -----------------------------------------------------------------------
	if err := ensureBonusTemplate(ctx, svc); err != nil {
		return err
	}
	bonusKey := ledger.NewIdempotencyKey("bonus-topup")
	if _, err := svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
		{TemplateCode: "fx_sell", Params: core.TemplateParams{ // the paid leg: 1 USDT
			HolderID: userID, CurrencyID: usdtID, IdempotencyKey: bonusKey + "-pay",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("1")},
		}},
		{TemplateCode: "credits_topup", Params: core.TemplateParams{ // 100 purchased + 20 bonus
			HolderID: userID, CurrencyID: creditsID, IdempotencyKey: bonusKey + "-issue",
			Amounts: map[string]decimal.Decimal{
				"purchased": decimal.RequireFromString("100"),
				"bonus":     decimal.RequireFromString("20"),
			},
		}},
	}); err != nil {
		return fmt.Errorf("recipe 2b bonus top-up: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after bonus top-up (+120 credits for 1 USDT)")

	// -----------------------------------------------------------------------
	// Recipe 4: spend credits with a budget. Reserve holds up to 50; the run
	// costs 32. Reserve/Settle is a soft lock over `available` and does NOT move
	// the balance — the actual debit is a separate journal we post on settle.
	// -----------------------------------------------------------------------
	rsv, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: userID, CurrencyID: creditsID,
		Amount:         decimal.RequireFromString("50"),
		IdempotencyKey: ledger.NewIdempotencyKey("run-budget"),
		ExpiresIn:      time.Hour,
	})
	if err != nil {
		return fmt.Errorf("recipe 4 reserve: %w", err)
	}
	fmt.Printf("  reserved 50 credits (id=%d, status=%s) — balance unchanged, available reduced\n", rsv.ID, rsv.Status)

	// Run finished, actual cost 32 → capture it and release the 18 remainder.
	if err := svc.Reserver().Settle(ctx, rsv.ID, decimal.RequireFromString("32")); err != nil {
		return fmt.Errorf("recipe 4 settle: %w", err)
	}
	// Post the actual spend so it hits the books (credits flow back to settlement).
	if err := ensureSpendTemplate(ctx, svc); err != nil {
		return err
	}
	if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "credits_spend", core.TemplateParams{
		HolderID: userID, CurrencyID: creditsID, IdempotencyKey: ledger.NewIdempotencyKey("run-spend"),
		Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("32")},
	}); err != nil {
		return fmt.Errorf("recipe 4 spend journal: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after spending 32 credits (settled from 50 budget)")

	// -----------------------------------------------------------------------
	// Recipe 5: cash 88 credits back to USDT at 100:1 (reverse FX two-leg).
	// -----------------------------------------------------------------------
	cashKey := ledger.NewIdempotencyKey("cash-out")
	cashMeta := map[string]string{"quote_id": "q-2", "fx_rate": "100"}
	if _, err := svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
		{TemplateCode: "fx_sell", Params: core.TemplateParams{
			HolderID: userID, CurrencyID: creditsID, IdempotencyKey: cashKey + "-sell",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("88")}, Metadata: cashMeta,
		}},
		{TemplateCode: "fx_buy", Params: core.TemplateParams{
			HolderID: userID, CurrencyID: usdtID, IdempotencyKey: cashKey + "-buy",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("0.88")}, Metadata: cashMeta,
		}},
	}); err != nil {
		return fmt.Errorf("recipe 5 cash out: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after cashing 88 credits back to 0.88 USDT")

	return nil
}

// ensureCurrency creates a currency if it doesn't already exist (idempotent).
func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (int64, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return 0, fmt.Errorf("list currencies: %w", err)
	}
	for _, c := range list {
		if c.Code == code {
			return c.ID, nil
		}
	}
	created, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: code, Name: name, Exponent: 18})
	if err != nil {
		return 0, fmt.Errorf("create currency %s: %w", code, err)
	}
	return created.ID, nil
}

// ensureBonusTemplate registers the credits_topup template (purchased + bonus)
// once. Bonus credits are funded from equity; purchased credits from settlement.
func ensureBonusTemplate(ctx context.Context, svc *ledger.Service) error {
	if _, err := svc.Templates().GetTemplate(ctx, "credits_topup"); err == nil {
		return nil // already registered
	}
	jt, err := ensureJournalType(ctx, svc, "credits_topup", "Credits Top-up with Bonus")
	if err != nil {
		return err
	}
	mw, st, eq, err := classIDs(ctx, svc, "main_wallet", "settlement", "equity")
	if err != nil {
		return err
	}
	_, err = svc.Templates().CreateTemplate(ctx, core.TemplateInput{
		Code: "credits_topup", Name: "Credits Top-up with Bonus", JournalTypeID: jt,
		Lines: []core.TemplateLineInput{
			{ClassificationID: mw, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "purchased", SortOrder: 1},
			{ClassificationID: st, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "purchased", SortOrder: 2},
			{ClassificationID: mw, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "bonus", SortOrder: 3},
			{ClassificationID: eq, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "bonus", SortOrder: 4},
		},
	})
	if err != nil {
		return fmt.Errorf("create credits_topup template: %w", err)
	}
	return nil
}

// ensureSpendTemplate registers credits_spend (credits leave the wallet back to
// settlement, reducing outstanding credit liability).
func ensureSpendTemplate(ctx context.Context, svc *ledger.Service) error {
	if _, err := svc.Templates().GetTemplate(ctx, "credits_spend"); err == nil {
		return nil
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
		Code: "credits_spend", Name: "Credits Spend", JournalTypeID: jt,
		Lines: []core.TemplateLineInput{
			{ClassificationID: st, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationID: mw, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	})
	if err != nil {
		return fmt.Errorf("create credits_spend template: %w", err)
	}
	return nil
}

func ensureJournalType(ctx context.Context, svc *ledger.Service, code, name string) (int64, error) {
	if jt, err := svc.JournalTypes().GetJournalTypeByCode(ctx, code); err == nil {
		return jt.ID, nil
	}
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name})
	if err != nil {
		return 0, fmt.Errorf("create journal type %s: %w", code, err)
	}
	return jt.ID, nil
}

func classIDs(ctx context.Context, svc *ledger.Service, a, b, c string) (int64, int64, int64, error) {
	ca, err := svc.Classifications().GetByCode(ctx, a)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get %s: %w", a, err)
	}
	cb, err := svc.Classifications().GetByCode(ctx, b)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get %s: %w", b, err)
	}
	cc, err := svc.Classifications().GetByCode(ctx, c)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("get %s: %w", c, err)
	}
	return ca.ID, cb.ID, cc.ID, nil
}

func printBalances(usdtBal, creditsBal func() (decimal.Decimal, error), label string) {
	usdt, err := usdtBal()
	if err != nil {
		log.Fatalf("read USDT balance: %v", err)
	}
	credits, err := creditsBal()
	if err != nil {
		log.Fatalf("read credits balance: %v", err)
	}
	fmt.Printf("%-52s USDT=%-8s CREDITS=%s\n", label+":", usdt, credits)
}
