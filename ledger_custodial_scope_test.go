package ledger_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// custodialScopeFixture funds one holder out of a custody account whose code
// is NOT "custodial", while a zero-balance classification actually named
// "custodial" also exists. A deployment shaped like this is the whole point of
// the option under test: with the default scope its custody position reads as
// zero and the ledger reports insolvency on a fully backed book.
type custodialScopeFixture struct {
	currencyUID string
	vaultCode   string
	amount      decimal.Decimal
}

func seedCustodialScopeFixture(t *testing.T, ctx context.Context, svc *ledger.Service) custodialScopeFixture {
	t.Helper()
	suffix := time.Now().UnixNano()
	amount := decimal.NewFromInt(100)
	const holder = int64(9501)

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("CSU_%d", suffix), Name: "Custodial Scope Unit", Exponent: 18,
	})
	require.NoError(t, err)

	wallet, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("cs_main_%d", suffix), Name: "Custodial Scope Main",
		NormalSide: core.NormalSideDebit, BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)

	// The custody account this deployment actually uses, under its own name.
	vaultCode := fmt.Sprintf("cs_vault_%d", suffix)
	vault, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: vaultCode, Name: "Custodial Scope Vault",
		NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	// A classification literally named "custodial", holding nothing. Its
	// presence is what makes the default scope resolve (so the control below
	// fails on the NUMBER, not on the empty-scope error) while still
	// contributing zero.
	_, err = svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: "custodial", Name: "Unused Custodial", NormalSide: core.NormalSideCredit, IsSystem: true,
	})
	require.NoError(t, err)

	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("cs_fund_%d", suffix), Name: "Custodial Scope Fund",
	})
	require.NoError(t, err)

	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("cs-fund"),
		Source:         "custodial-scope-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: wallet.UID, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: vault.UID, EntryType: core.EntryTypeCredit, Amount: amount},
		},
	})
	require.NoError(t, err)

	return custodialScopeFixture{currencyUID: cur.UID, vaultCode: vaultCode, amount: amount}
}

// TestService_WithCustodialClassCodes_ReachesSolvencyCheck pins the facade
// exit for the custodial scope W1-sign made injectable. Without an option
// here, "what counts as a custodied asset" was reachable only by constructing
// postgres.NewPlatformBalanceStore directly -- the one thing CLAUDE.md tells
// consumers never to do -- so every facade consumer was pinned to the default
// set regardless of what its accounts are called.
//
// The control and the assertion differ only in whether the option was passed,
// so this fails if ledger.New stops applying it: the book is identically
// backed in both halves, and only the scope decides whether the ledger can
// see it.
func TestService_WithCustodialClassCodes_ReachesSolvencyCheck(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svcDefault, err := ledger.New(pool)
	require.NoError(t, err)
	fx := seedCustodialScopeFixture(t, ctx, svcDefault)

	// Control: the default scope resolves (a "custodial" classification does
	// exist) but sees none of the money, because this deployment keeps it
	// somewhere else.
	base, err := svcDefault.SolvencyChecker().SolvencyCheck(ctx, fx.currencyUID)
	require.NoError(t, err)
	require.True(t, base.Custodial.IsZero(),
		"control: under the default scope this deployment's custody account is invisible, got %s", base.Custodial)
	require.True(t, base.Liability.Equal(fx.amount))
	require.False(t, base.Solvent,
		"control: a fully backed book reads as insolvent until the scope names the right account")

	// The option, through the facade a consumer actually uses.
	svcScoped, err := ledger.New(pool, ledger.WithCustodialClassCodes(fx.vaultCode))
	require.NoError(t, err)

	scoped, err := svcScoped.SolvencyChecker().SolvencyCheck(ctx, fx.currencyUID)
	require.NoError(t, err)
	require.True(t, scoped.Custodial.Equal(fx.amount),
		"ledger.WithCustodialClassCodes is not reaching the PlatformBalanceStore: expected custodial %s, got %s",
		fx.amount, scoped.Custodial)
	require.True(t, scoped.Liability.Equal(fx.amount))
	require.True(t, scoped.Solvent, "with the right scope the same book is solvent; margin=%s", scoped.Margin)
	require.True(t, scoped.Margin.IsZero())
}

// TestService_WithCustodialClassCodes_FailsLoudOnAScopeThatMatchesNothing
// pins the fail-closed half. A scope naming classifications that do not exist
// can only ever produce Custodial = 0, which reads on the wire exactly like a
// genuinely empty custody position -- an insolvency alarm nailed to ON, which
// is the same "degraded looks identical to broken" shape I-54 exists for. It
// must be an error instead.
//
// It surfaces at read time rather than at ledger.New because New performs no
// I/O; asserting that placement here is what keeps a future "validate in New"
// change from being made silently.
func TestService_WithCustodialClassCodes_FailsLoudOnAScopeThatMatchesNothing(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	fx := seedCustodialScopeFixture(t, ctx, svc)

	bogus := fmt.Sprintf("no_such_custody_%d", time.Now().UnixNano())
	scoped, err := ledger.New(pool, ledger.WithCustodialClassCodes(bogus))
	require.NoError(t, err, "construction performs no I/O, so the bad scope cannot be caught here")

	report, err := scoped.SolvencyChecker().SolvencyCheck(ctx, fx.currencyUID)
	require.Error(t, err, "a custodial scope matching no classification must be rejected, not reported as Custodial = 0")
	require.ErrorIs(t, err, core.ErrInvalidInput)
	require.Nil(t, report)
	require.Contains(t, err.Error(), bogus, "the error must name the scope that matched nothing")
}

// TestService_WithCustodialClassCodes_EmptyCallIsIgnored pins the guard
// against the option emptying the scope by accident:
// WithCustodialClassCodes() with no arguments must leave the default in
// place, not install an empty set that would then fail every solvency read.
func TestService_WithCustodialClassCodes_EmptyCallIsIgnored(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool, ledger.WithCustodialClassCodes())
	require.NoError(t, err)
	fx := seedCustodialScopeFixture(t, ctx, svc)

	report, err := svc.SolvencyChecker().SolvencyCheck(ctx, fx.currencyUID)
	require.NoError(t, err, "an argument-less call must fall back to the default scope, not to an empty one")
	require.True(t, report.Custodial.IsZero())
}

// TestService_WithCustodialClassCodes_SurvivesRunInTx pins that the scope is
// not silently dropped on the transaction-bound clone -- the same class of
// bug as Onchain() returning nil on a clone (I-54 property 5): a solvency
// read composed inside a caller's transaction must answer with the scope the
// Service was configured with, not with the shipped default.
func TestService_WithCustodialClassCodes_SurvivesRunInTx(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	fx := seedCustodialScopeFixture(t, ctx, svc)

	scoped, err := ledger.New(pool, ledger.WithCustodialClassCodes(fx.vaultCode))
	require.NoError(t, err)

	require.NoError(t, scoped.RunInTx(ctx, func(tx *ledger.Service) error {
		report, checkErr := tx.SolvencyChecker().SolvencyCheck(ctx, fx.currencyUID)
		require.NoError(t, checkErr)
		require.True(t, report.Custodial.Equal(fx.amount),
			"the clone must keep the configured custodial scope, got %s", report.Custodial)
		return nil
	}))
}
