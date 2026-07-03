package postgres_test

// I-16 pin tests: any already-posted entry's amount decimal places must not
// exceed its currency's exponent. See docs/INVARIANTS.md.

import (
	"context"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/presets"
)

// TestPrecision_PostJournal_RejectsOverPrecisionAmount pins the core write
// path (I-16): a JPY-like currency with exponent=0 must reject any entry
// carrying fractional units.
func TestPrecision_PostJournal_RejectsOverPrecisionAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store := postgres.NewLedgerStore(pool)

	jpyID := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY-PREC", "Japanese Yen", 0)
	mainWallet := postgrestest.SeedClassification(t, pool, "main_wallet_jpy_prec", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_jpy_prec", "Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, "test_jpy_prec", "Test JPY Precision")

	userID := int64(9001)

	_, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("jpy-half"),
		Source:         "precision-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: jpyID, ClassificationUID: mainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("0.5")},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: jpyID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("0.5")},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrPrecisionExceeded)
}

// TestPrecision_PostJournal_AcceptsWholeYen confirms exponent=0 doesn't
// reject valid whole-unit amounts — the check is about decimal places, not a
// blanket rejection of the currency.
func TestPrecision_PostJournal_AcceptsWholeYen(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store := postgres.NewLedgerStore(pool)

	jpyID := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY-OK", "Japanese Yen OK", 0)
	mainWallet := postgrestest.SeedClassification(t, pool, "main_wallet_jpy_ok", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_jpy_ok", "Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, "test_jpy_ok", "Test JPY OK")

	userID := int64(9002)

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("jpy-whole"),
		Source:         "precision-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: jpyID, ClassificationUID: mainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(500)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: jpyID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(500)},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, j)
}

// TestPrecision_PostJournal_DefaultExponentStillAllowsFractionalAmounts is a
// regression guard: currencies created without an explicit exponent (i.e.
// pre-migration-027 rows, or any SeedCurrency call in the existing suite)
// default to exponent=18 and must keep accepting the fractional amounts the
// rest of the test suite already relies on.
func TestPrecision_PostJournal_DefaultExponentStillAllowsFractionalAmounts(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	store := postgres.NewLedgerStore(pool)

	usdtID := postgrestest.SeedCurrency(t, pool, "USDT-PREC-DEFAULT", "Tether Default Exponent")
	mainWallet := postgrestest.SeedClassification(t, pool, "main_wallet_usdt_default", "Main Wallet", "debit", false)
	custodial := postgrestest.SeedClassification(t, pool, "custodial_usdt_default", "Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, "test_usdt_default", "Test USDT Default")

	userID := int64(9003)

	j, err := store.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("usdt-fractional"),
		Source:         "precision-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: usdtID, ClassificationUID: mainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.RequireFromString("100.123456789012345678")},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: usdtID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.RequireFromString("100.123456789012345678")},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, j)
}

// TestPrecision_Reserve_RejectsOverPrecisionAmount pins the Reserve write
// path, which does not flow through PostJournal and therefore needs its own
// enforcement point (see postgres/reserver_store.go).
func TestPrecision_Reserve_RejectsOverPrecisionAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	reserver := postgres.NewReserverStore(pool, ledgerStore)

	jpyID := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY-RSV", "Japanese Yen Reserve", 0)

	_, err := reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  int64(9004),
		CurrencyUID:    jpyID,
		Amount:         decimal.RequireFromString("12.5"),
		IdempotencyKey: postgrestest.UniqueKey("jpy-reserve"),
		ExpiresIn:      time.Hour,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrPrecisionExceeded)
}

// TestPrecision_Reserve_AcceptsWholeYen mirrors
// TestPrecision_PostJournal_AcceptsWholeYen for the Reserve path.
func TestPrecision_Reserve_AcceptsWholeYen(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	ledgerStore := postgres.NewLedgerStore(pool)
	reserver := postgres.NewReserverStore(pool, ledgerStore)

	jpyID := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY-RSV-OK", "Japanese Yen Reserve OK", 0)
	mainWallet := postgrestest.SeedClassificationWithRole(t, pool, "main_wallet_jpy_rsv_ok", "Main Wallet", "debit", false, "available")
	custodial := postgrestest.SeedClassification(t, pool, "custodial_jpy_rsv_ok", "Custodial", "credit", true)
	jt := postgrestest.SeedJournalType(t, pool, "test_jpy_rsv_ok", "Test JPY Reserve OK")

	userID := int64(9005)

	// Fund the account first so Reserve's availability check passes.
	_, err := ledgerStore.PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt,
		IdempotencyKey: postgrestest.UniqueKey("jpy-fund"),
		Source:         "precision-test",
		Entries: []core.EntryInput{
			{AccountHolder: userID, CurrencyUID: jpyID, ClassificationUID: mainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1000)},
			{AccountHolder: core.SystemAccountHolder(userID), CurrencyUID: jpyID, ClassificationUID: custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1000)},
		},
	})
	require.NoError(t, err)

	_, err = reserver.Reserve(ctx, core.ReserveInput{
		AccountHolder:  userID,
		CurrencyUID:    jpyID,
		Amount:         decimal.NewFromInt(500),
		IdempotencyKey: postgrestest.UniqueKey("jpy-reserve-ok"),
		ExpiresIn:      time.Hour,
	})
	require.NoError(t, err)
}

// TestPrecision_Pending_RejectsOverPrecisionAmount pins AddPending —
// PendingStore funnels through LedgerStore.PostJournal, so this is a
// regression guard confirming that composition still enforces I-16 rather
// than bypassing it via a different code path.
func TestPrecision_Pending_RejectsOverPrecisionAmount(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	cs := postgres.NewClassificationStore(pool)
	ls := postgres.NewLedgerStore(pool)
	ts := postgres.NewTemplateStore(pool)
	require.NoError(t, presets.InstallPendingBundle(ctx, cs, cs, ts))

	jpyID := postgrestest.SeedCurrencyWithExponent(t, pool, "JPY-PEND", "Japanese Yen Pending", 0)
	ps := postgres.NewPendingStore(pool, ls, cs)

	_, err := ps.AddPending(ctx, core.AddPendingInput{
		AccountHolder:  int64(9006),
		CurrencyUID:    jpyID,
		Amount:         decimal.RequireFromString("0.5"),
		IdempotencyKey: postgrestest.UniqueKey("jpy-pending"),
		Source:         "precision-test",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrPrecisionExceeded)
}
