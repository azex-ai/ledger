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
//	# optional: migrations on their own credential (see docs/RUNBOOK.md "Database roles")
//	export MIGRATE_DATABASE_URL="postgres://ledger_owner:pass@localhost:5432/ledger_dev?sslmode=disable"
//	go run ./examples/credits-topup
package main

import (
	"context"
	"errors"
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
	// Migrations run on their own credential. Migrate switches its own
	// connection to ledger_owner rather than granting the credential
	// ledger_owner's privileges, so this pool inherits nothing while a
	// migration run is in flight -- but a credential that can reach
	// ledger_owner at all is still not one to serve traffic on: any session
	// holding it can SET ROLE to that role deliberately. See docs/RUNBOOK.md
	// "Database roles".
	migrateURL := os.Getenv("MIGRATE_DATABASE_URL")
	if migrateURL == "" {
		migrateURL = dbURL
		log.Printf("warning: MIGRATE_DATABASE_URL is unset, so migrations run on DATABASE_URL. " +
			"That credential can act as ledger_owner -- able to drop the append-only guards -- which is not something " +
			"a serving pool should be able to do. Acceptable for a local example, not for production.")
	}
	// postgres.Migrate uses golang-migrate's pgx5 driver (pgx5:// scheme);
	// pgxpool.New wants postgres://. Accept a standard postgres:// URL and
	// translate for the migrator.
	if rest, ok := strings.CutPrefix(migrateURL, "postgres://"); ok {
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
	usdtUID, err := ensureCurrency(ctx, svc, "USDT", "Tether USD")
	if err != nil {
		return err
	}
	creditsUID, err := ensureCurrency(ctx, svc, "CREDITS", "Platform Credits")
	if err != nil {
		return err
	}

	mainWallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return fmt.Errorf("get main_wallet: %w", err)
	}
	creditsBal := func() (decimal.Decimal, error) {
		return svc.BalanceReader().GetBalance(ctx, userID, creditsUID, mainWallet.UID)
	}
	usdtBal := func() (decimal.Decimal, error) {
		return svc.BalanceReader().GetBalance(ctx, userID, usdtUID, mainWallet.UID)
	}

	// -----------------------------------------------------------------------
	// Seed: give the user 10 USDT to spend (a confirmed deposit).
	// -----------------------------------------------------------------------
	if _, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: userID, CurrencyUID: usdtUID, IdempotencyKey: ledger.NewIdempotencyKey("seed-usdt"),
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
			HolderID: userID, CurrencyUID: usdtUID, IdempotencyKey: buyKey + "-sell",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("1")}, Metadata: buyMeta,
		}},
		{TemplateCode: "fx_buy", Params: core.TemplateParams{
			HolderID: userID, CurrencyUID: creditsUID, IdempotencyKey: buyKey + "-buy",
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
			HolderID: userID, CurrencyUID: usdtUID, IdempotencyKey: bonusKey + "-pay",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("1")},
		}},
		{TemplateCode: "credits_topup", Params: core.TemplateParams{ // 100 purchased + 20 bonus
			HolderID: userID, CurrencyUID: creditsUID, IdempotencyKey: bonusKey + "-issue",
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
		AccountHolder: userID, CurrencyUID: creditsUID,
		Amount:         decimal.RequireFromString("50"),
		IdempotencyKey: ledger.NewIdempotencyKey("run-budget"),
		ExpiresIn:      time.Hour,
	})
	if err != nil {
		return fmt.Errorf("recipe 4 reserve: %w", err)
	}
	fmt.Printf("  reserved 50 credits (uid=%s, status=%s) — balance unchanged, available reduced\n", rsv.UID, rsv.Status)

	// Run finished, actual cost 32. Settle only closes the hold -- it writes
	// no entries, the actual debit is the credits_spend journal below -- and
	// the two must land together. Settle and the journal run inside one
	// RunInTx: a crash between them would otherwise free the hold and spend
	// nobody's credits, while the ledger reports success because from its
	// side nothing failed (the same failure examples/billing used to teach).
	//
	// ExecuteTemplate called directly inside RunInTx (below) always posts
	// auth_status=unsigned_tx_mode -- there is no point inside an already
	// open transaction where calling out to an Attestor would not itself be
	// the "external call inside a DB transaction" financial.md forbids. If
	// this service were constructed WithAttestor and something downstream
	// uses RequireVerifiedBalance on this dimension, that gate would refuse
	// to pay it out. The fix is svc.AuthorizeTemplate BEFORE RunInTx opens,
	// then tx.JournalWriter().PostAuthorized(...) inside it instead of
	// ExecuteTemplate -- see examples/tamper-evident's appendix for a
	// runnable, asserted demonstration of both paths side by side.
	if err := ensureSpendTemplate(ctx, svc); err != nil {
		return err
	}
	settleKey := ledger.NewIdempotencyKey("run-settle")
	spendKey := ledger.NewIdempotencyKey("run-spend")
	if err := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if err := tx.Reserver().Settle(ctx, core.SettleInput{
			ReservationUID: rsv.UID,
			Amount:         decimal.RequireFromString("32"),
			IdempotencyKey: settleKey,
		}); err != nil {
			return fmt.Errorf("settle: %w", err)
		}
		if _, err := tx.JournalWriter().ExecuteTemplate(ctx, "credits_spend", core.TemplateParams{
			HolderID: userID, CurrencyUID: creditsUID, IdempotencyKey: spendKey,
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("32")},
		}); err != nil {
			return fmt.Errorf("spend journal: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("recipe 4 settle and spend: %w", err)
	}

	if got, err := creditsBal(); err != nil {
		return fmt.Errorf("get balance after spend: %w", err)
	} else if want := decimal.RequireFromString("188"); !got.Equal(want) {
		// 100 (recipe 1) + 120 (recipe 2b) - 32 (this spend) = 188. A number
		// that only matches "settled but never spent" here means the charge
		// did not land -- this is the check examples/billing was missing.
		return fmt.Errorf("credits balance is %s, expected %s -- the spend journal did not land", got, want)
	}
	printBalances(usdtBal, creditsBal, "after spending 32 credits (settled from 50 budget)")

	// -----------------------------------------------------------------------
	// Recipe 5: cash 88 credits back to USDT at 100:1 (reverse FX two-leg).
	// -----------------------------------------------------------------------
	cashKey := ledger.NewIdempotencyKey("cash-out")
	cashMeta := map[string]string{"quote_id": "q-2", "fx_rate": "100"}
	if _, err := svc.TemplateBatchExecutor().ExecuteTemplateBatch(ctx, []core.TemplateExecutionRequest{
		{TemplateCode: "fx_sell", Params: core.TemplateParams{
			HolderID: userID, CurrencyUID: creditsUID, IdempotencyKey: cashKey + "-sell",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("88")}, Metadata: cashMeta,
		}},
		{TemplateCode: "fx_buy", Params: core.TemplateParams{
			HolderID: userID, CurrencyUID: usdtUID, IdempotencyKey: cashKey + "-buy",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("0.88")}, Metadata: cashMeta,
		}},
	}); err != nil {
		return fmt.Errorf("recipe 5 cash out: %w", err)
	}
	printBalances(usdtBal, creditsBal, "after cashing 88 credits back to 0.88 USDT")

	return nil
}

// ensureCurrency creates a currency if it doesn't already exist (idempotent).
func ensureCurrency(ctx context.Context, svc *ledger.Service, code, name string) (string, error) {
	list, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return "", fmt.Errorf("list currencies: %w", err)
	}
	const exponent = int32(6)
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

// ensureBonusTemplate registers the credits_topup template (purchased + bonus)
// once. Bonus credits are funded from equity; purchased credits from settlement.
func ensureBonusTemplate(ctx context.Context, svc *ledger.Service) error {
	if _, err := svc.Templates().GetTemplate(ctx, "credits_topup"); err == nil {
		return nil // already registered
	} else if !errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("get template credits_topup: %w", err)
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
		Code: "credits_topup", Name: "Credits Top-up with Bonus", JournalTypeUID: jt,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: mw, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "purchased", SortOrder: 1},
			{ClassificationUID: st, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "purchased", SortOrder: 2},
			{ClassificationUID: mw, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "bonus", SortOrder: 3},
			{ClassificationUID: eq, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "bonus", SortOrder: 4},
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
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{Code: code, Name: name})
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
