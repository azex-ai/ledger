// Example: reuse configured accounts and templates for deposits, fees, points
// and gifts. This is a local bookkeeping fixture, not an external trade.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

const (
	userID    int64 = 2010
	eventRoot       = "configured-demo-v1"
)

// These are existing ledger configuration types, not a new configuration DSL.
var currencies = []core.CurrencyInput{
	{Code: "USDC", Name: "USD Coin", Exponent: 6},
	{Code: "CREDITS", Name: "Credits", Exponent: 6},
	{Code: "POINTS", Name: "Reward Points", Exponent: 0},
	{Code: "ROSE", Name: "Rose Gift", Exponent: 0},
}

func purchaseRate() core.FixedRate {
	return core.FixedRate{SourceCode: "USDC", TargetCode: "CREDITS",
		Rate: decimal.NewFromInt(1000), Version: "catalog-v1", Rounding: core.RoundDown}
}

func giftRate() core.FixedRate {
	return core.FixedRate{SourceCode: "CREDITS", TargetCode: "ROSE",
		Rate: decimal.RequireFromString("0.1"), Version: "catalog-v1", Rounding: core.RoundDown}
}

// POINTS is a count of issued rewards. Its system counterpart records issuance,
// not cash custody or a promise that points can be redeemed for money.
func pointsBundle() presets.TemplateBundle {
	return presets.TemplateBundle{
		Classifications: []presets.ClassificationPreset{
			{Code: "main_wallet", Name: "Main Wallet", NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable},
			{Code: "points_issued", Name: "Points Issued", NormalSide: core.NormalSideCredit, IsSystem: true},
		},
		JournalTypes: []presets.JournalTypePreset{
			{Code: "points_issue", Name: "Points Issue", DisplayLabel: "Points", HolderKind: core.HolderTxKindOther},
		},
		Templates: []presets.TemplatePreset{{
			Code: "points_issue", Name: "Points Issue", JournalTypeCode: "points_issue",
			Lines: []presets.TemplateLinePreset{
				{ClassificationCode: "main_wallet", EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
				{ClassificationCode: "points_issued", EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
			},
		}},
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	dbURL, migrateURL := os.Getenv("DATABASE_URL"), os.Getenv("MIGRATE_DATABASE_URL")
	if dbURL == "" || migrateURL == "" {
		return errors.New("DATABASE_URL (runtime) and MIGRATE_DATABASE_URL (migration credential) are required")
	}
	if err := ledger.Migrate(migrateURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	svc, err := ledger.New(pool)
	if err != nil {
		return err
	}
	if err := svc.AssertRuntimeRole(ctx); err != nil {
		return err
	}
	units, err := setup(ctx, svc)
	if err != nil {
		return err
	}
	journalUIDs, err := scenario(ctx, svc, units)
	if err != nil {
		return err
	}
	if err := checkJournalBalances(ctx, svc, journalUIDs); err != nil {
		return err
	}
	if err := checkBalances(ctx, svc, units, finalBalances()); err != nil {
		return err
	}
	fmt.Println("Verified native wallet quantities: 74 USDC, 980 CREDITS, 100 POINTS, 2 ROSE.")
	fmt.Println("Verified fee memo: 25 USDC; configured system balances and every journal/currency balance.")
	fmt.Println("Replay the same fixture event IDs with their original configuration; no external trade was executed.")
	return nil
}

func setup(ctx context.Context, svc *ledger.Service) (map[string]string, error) {
	for _, bundle := range []presets.TemplateBundle{
		presets.DepositBundle(), presets.FeeBundle(), presets.FXBundle(), pointsBundle(),
	} {
		if err := presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), bundle); err != nil {
			return nil, err
		}
	}
	existing, err := svc.Currencies().ListCurrencies(ctx, false)
	if err != nil {
		return nil, err
	}
	units := make(map[string]string, len(currencies))
	for _, input := range currencies {
		for _, current := range existing {
			if current.Code == input.Code {
				if !current.IsActive || current.Exponent != input.Exponent || current.Name != input.Name {
					return nil, fmt.Errorf("currency %s differs from fixture configuration: %w", input.Code, core.ErrConflict)
				}
				units[input.Code] = current.UID
			}
		}
		if units[input.Code] == "" {
			created, err := svc.Currencies().CreateCurrency(ctx, input)
			if err != nil {
				return nil, err
			}
			units[input.Code] = created.UID
		}
	}
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return nil, err
	}
	for _, input := range currencies {
		if _, err := svc.AccountPolicies().SetPolicy(ctx, core.AccountPolicyInput{
			AccountHolder: userID, CurrencyUID: units[input.Code], ClassificationUID: wallet.UID,
			Status: core.AccountPolicyStatusActive, EnforceMinBalance: true, MinBalance: decimal.Zero,
			Note: "Configured ledger example: no overdraft",
		}); err != nil {
			return nil, err
		}
	}
	return units, nil
}

func params(currency, event, amount string) core.TemplateParams {
	return core.TemplateParams{
		HolderID: userID, CurrencyUID: currency, IdempotencyKey: eventRoot + ":" + event,
		Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString(amount)},
		Source:  "configured-ledger-example",
	}
}

// Each event is independently replayable; the whole demonstration is not one
// transaction. Exchange itself atomically commits both currency legs and its hold.
func scenario(ctx context.Context, svc *ledger.Service, units map[string]string) ([]string, error) {
	deposit, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", params(units["USDC"], "deposit", "100"))
	if err != nil {
		return nil, err
	}
	fee, err := svc.JournalWriter().ExecuteTemplate(ctx, "fee_charge", params(units["USDC"], "fee", "25"))
	if err != nil {
		return nil, err
	}
	purchase, err := svc.Exchange(ctx, ledger.ExchangeInput{
		HolderID: userID, SourceCurrencyUID: units["USDC"], TargetCurrencyUID: units["CREDITS"],
		Quantity: decimal.NewFromInt(1), Rate: purchaseRate(), FundingUID: deposit.UID,
		IdempotencyKey: eventRoot + ":purchase", Source: "configured-ledger-example",
	})
	if err != nil {
		return nil, err
	}
	points, err := svc.JournalWriter().ExecuteTemplate(ctx, "points_issue", params(units["POINTS"], "reward", "100"))
	if err != nil {
		return nil, err
	}
	gift, err := exchangeGift(ctx, svc, units, giftRate())
	if err != nil {
		return nil, err
	}
	return []string{deposit.UID, fee.UID, purchase.SellJournalUID, purchase.BuyJournalUID,
		points.UID, gift.SellJournalUID, gift.BuyJournalUID}, nil
}

func exchangeGift(ctx context.Context, svc *ledger.Service, units map[string]string, rate core.FixedRate) (*ledger.ExchangeResult, error) {
	return svc.Exchange(ctx, ledger.ExchangeInput{
		HolderID: userID, SourceCurrencyUID: units["CREDITS"], TargetCurrencyUID: units["ROSE"],
		Quantity: decimal.NewFromInt(20), Rate: rate,
		IdempotencyKey: eventRoot + ":gift", Source: "configured-ledger-example",
	})
}

var errEconomics = errors.New("configured economic expectation failed")

type balanceExpectation struct {
	holder                    int64
	currency, class, quantity string
}

// Independent business oracle: literals from the scenario, never calculated
// from the configured templates or rates that this acceptance check evaluates.
func finalBalances() []balanceExpectation {
	system := core.SystemAccountHolder(userID)
	return []balanceExpectation{
		{userID, "USDC", "main_wallet", "74"},
		{userID, "CREDITS", "main_wallet", "980"},
		{userID, "POINTS", "main_wallet", "100"},
		{userID, "ROSE", "main_wallet", "2"},
		{userID, "USDC", "fee_expense", "25"},
		{system, "USDC", "custodial", "75"},
		{system, "USDC", "fees", "25"},
		{system, "USDC", "settlement", "-1"},
		{system, "CREDITS", "settlement", "980"},
		{system, "POINTS", "points_issued", "100"},
		{system, "ROSE", "settlement", "2"},
	}
}

func checkBalances(ctx context.Context, svc *ledger.Service, units map[string]string, expected []balanceExpectation) error {
	for _, want := range expected {
		class, err := svc.Classifications().GetByCode(ctx, want.class)
		if err != nil {
			return err
		}
		got, err := svc.BalanceReader().GetBalance(ctx, want.holder, units[want.currency], class.UID)
		if err != nil {
			return err
		}
		if !got.Equal(decimal.RequireFromString(want.quantity)) {
			return fmt.Errorf("%w: holder=%d %s/%s got %s, want %s",
				errEconomics, want.holder, want.currency, want.class, got, want.quantity)
		}
	}
	return nil
}

// Balance is checked for each persisted journal AND currency; native quantities
// or normal-side account balances from different currencies are never summed.
func checkJournalBalances(ctx context.Context, svc *ledger.Service, journalUIDs []string) error {
	for _, uid := range journalUIDs {
		_, entries, err := svc.Queries().GetJournal(ctx, uid)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return fmt.Errorf("journal %s has no entries", uid)
		}
		net := make(map[string]decimal.Decimal)
		for _, entry := range entries {
			switch entry.EntryType {
			case core.EntryTypeDebit:
				net[entry.CurrencyUID] = net[entry.CurrencyUID].Add(entry.Amount)
			case core.EntryTypeCredit:
				net[entry.CurrencyUID] = net[entry.CurrencyUID].Sub(entry.Amount)
			default:
				return fmt.Errorf("journal %s has unknown entry type %q", uid, entry.EntryType)
			}
		}
		for currency, delta := range net {
			if !delta.IsZero() {
				return fmt.Errorf("journal %s currency %s: debit minus credit = %s", uid, currency, delta)
			}
		}
	}
	return nil
}
