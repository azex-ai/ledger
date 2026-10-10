// A local credits fixture: pre-authorize a journal, then atomically charge and
// settle using existing ledger APIs. See README.md for the signature boundary.
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/presets"
)

const holder int64 = 13001

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	dbURL, migrateURL := os.Getenv("DATABASE_URL"), os.Getenv("MIGRATE_DATABASE_URL")
	if dbURL == "" || migrateURL == "" {
		return fmt.Errorf("DATABASE_URL and MIGRATE_DATABASE_URL are required; use a fresh example database")
	}
	if err := ledger.Migrate(migrateURL); err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	// This executable requires a fresh database: its random development key
	// lives only in memory. A deployed host must retain its signing key outside
	// the database credential's failure domain, and retain historical public keys.
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	attestor, verifier, err := authdev.NewLocalAttestor(seed, "signed-capture-demo")
	if err != nil {
		return err
	}
	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	if err != nil {
		return err
	}
	if err := svc.AssertRuntimeRole(ctx); err != nil {
		return err
	}
	currency, wallet, err := setup(ctx, svc)
	if err != nil {
		return err
	}
	if err := fund(ctx, svc, currency); err != nil {
		return err
	}
	r, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: holder, CurrencyUID: currency, Amount: decimal.NewFromInt(60),
		IdempotencyKey: "signed-capture:budget:job-1", ExpiresIn: time.Hour,
		RequireVerifiedBalance: true, // pool mode: verifier finishes before the DB transaction
	})
	if err != nil {
		return err
	}
	// This fixture stands in for a completed provider usage result. Any real
	// provider call happens outside the transaction, and is not atomic with it.
	event := usageEvent{ID: "job-1:completed", Amount: decimal.NewFromInt(20), RecordedAt: time.Now().UTC()}
	journal, err := capture(ctx, svc, r, event)
	if err != nil {
		return err
	}
	replayed, err := capture(ctx, svc, r, event)
	if err != nil {
		return err
	}
	if replayed.UID != journal.UID {
		return fmt.Errorf("replay created a second journal")
	}
	verified, err := svc.VerifiedBalanceReader().VerifiedBalance(ctx, holder, currency, wallet)
	if err != nil {
		return err
	}
	held, err := svc.Reserver().HeldAmount(ctx, holder, currency)
	if err != nil {
		return err
	}
	if !verified.Equal(decimal.NewFromInt(80)) || !held.IsZero() {
		return fmt.Errorf("unexpected verified balance=%s ordinary hold=%s", verified, held)
	}
	_, err = svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: holder, CurrencyUID: currency, Amount: decimal.NewFromInt(21),
		IdempotencyKey: "signed-capture:gate-probe", RequireVerifiedBalance: true,
	})
	if !errors.Is(err, core.ErrInsufficientBalance) {
		return fmt.Errorf("expected gated reserve refusal, got %v", err)
	}
	fmt.Printf("signed charge=%s; replay returned the same journal\n", journal.UID)
	fmt.Printf("verified balance=%s; ordinary hold=%s\n", verified, held)
	fmt.Println("gated reserve 21 refused: 80 balance - 60 unexpired unsigned hold = 20")
	return nil
}

// setup intentionally creates a new currency/template: repeat executable runs
// against the same database fail instead of silently replacing a signing key.
func setup(ctx context.Context, svc *ledger.Service) (currency, wallet string, err error) {
	if err := presets.InstallTemplateBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates(), presets.DepositBundle()); err != nil {
		return "", "", err
	}
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{Code: "SCREDITS", Name: "Signed demo credits", Exponent: 2})
	if err != nil {
		return "", "", err
	}
	w, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	if err != nil {
		return "", "", err
	}
	custodial, err := svc.Classifications().GetByCode(ctx, "custodial")
	if err != nil {
		return "", "", err
	}
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: chargeTemplate, Name: "Demo credits redeemed", DisplayLabel: "Usage", HolderKind: core.HolderTxKindFee,
	})
	if err != nil {
		return "", "", err
	}
	_, err = svc.Templates().CreateTemplate(ctx, core.TemplateInput{
		Code: chargeTemplate, Name: "Signed credits spend", JournalTypeUID: jt.UID,
		Lines: []core.TemplateLineInput{
			{ClassificationUID: custodial.UID, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 1},
			{ClassificationUID: w.UID, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 2},
		},
	})
	if err != nil {
		return "", "", err
	}
	_, err = svc.AccountPolicies().SetPolicy(ctx, core.AccountPolicyInput{
		AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: w.UID,
		Status: core.AccountPolicyStatusActive, EnforceMinBalance: true, MinBalance: decimal.Zero,
	})
	return cur.UID, w.UID, err
}

func fund(ctx context.Context, svc *ledger.Service, currency string) error {
	_, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
		HolderID: holder, CurrencyUID: currency, IdempotencyKey: "signed-capture:fixture-credit",
		Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(100)}, Source: "signed-capture-fixture",
	})
	return err
}
