package ledger_test

// Pin for B-m5: the process-wide dimension cache must never publish config
// rows that only exist inside somebody's open transaction. Driven from the
// real consumption entry point (ledger.New(pool) + RunInTx), because the
// bug needed exactly that composition: a tx-bound clone inherits the shared
// cache pointer from its pool-backed parent.

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// TestDimCache_RolledBackCurrencyIsNotPublished pins the whole failure mode:
// a currency created inside a RunInTx that then rolls back must not remain
// resolvable afterwards. Before the fix the in-transaction refresh wrote the
// uncommitted row into the pool-keyed shared cache; the rollback burnt the
// BIGSERIAL id but left the cache entry, and because a cache HIT never
// re-validates, every later request on any connection resolved that uid to a
// nonexistent id and failed on a foreign key — for the life of the process.
func TestDimCache_RolledBackCurrencyIsNotPublished(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	jtUID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	userCls := postgrestest.SeedClassification(t, pool, "main_wallet", "Wallet", "debit", false)
	systemCls := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	sentinel := errors.New("caller rolls back")
	var doomedUID string

	txErr := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		cur, err := tx.Currencies().CreateCurrency(ctx, core.CurrencyInput{
			Code: "DOOMED", Name: "Doomed Coin", Exponent: 18,
		})
		if err != nil {
			return err
		}
		doomedUID = cur.UID

		// Resolve it inside the same transaction: this is what triggered the
		// refresh that published the uncommitted row. It must still work —
		// the fix keeps in-transaction resolution, it only stops publishing.
		if _, err := tx.JournalWriter().PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtUID,
			IdempotencyKey: postgrestest.UniqueKey("dimcache-doomed"),
			Entries: []core.EntryInput{
				{AccountHolder: 1, CurrencyUID: cur.UID, ClassificationUID: userCls, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(5)},
				{AccountHolder: -1, CurrencyUID: cur.UID, ClassificationUID: systemCls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(5)},
			},
		}); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, txErr, sentinel)
	require.NotEmpty(t, doomedUID)

	// From the pool, on a fresh connection: the currency does not exist, so
	// resolving it must be ErrNotFound. Pre-fix this came back as a foreign
	// key violation from the shared cache's phantom id.
	_, err = svc.BalanceReader().GetBalance(ctx, 1, doomedUID, userCls)
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound,
		"a rolled-back currency must resolve as not-found, not as a phantom id from the shared dimension cache (got %v)", err)

	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jtUID,
		IdempotencyKey: postgrestest.UniqueKey("dimcache-phantom"),
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: doomedUID, ClassificationUID: userCls, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(5)},
			{AccountHolder: -1, CurrencyUID: doomedUID, ClassificationUID: systemCls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(5)},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrNotFound,
		"posting against a rolled-back currency must be refused at the dimension boundary, not by a foreign key deep in the write (got %v)", err)
}

// TestDimCache_CommittedCurrencyCreatedInTxIsResolvable is the companion the
// fix must not break: creating a config row and using it in the SAME
// transaction still works, and it is visible to the pool once committed.
func TestDimCache_CommittedCurrencyCreatedInTxIsResolvable(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	jtUID := postgrestest.SeedJournalType(t, pool, "transfer", "Transfer")
	userCls := postgrestest.SeedClassification(t, pool, "main_wallet", "Wallet", "debit", false)
	systemCls := postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true)

	var freshUID string
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		cur, err := tx.Currencies().CreateCurrency(ctx, core.CurrencyInput{
			Code: "FRESH", Name: "Fresh Coin", Exponent: 18,
		})
		if err != nil {
			return err
		}
		freshUID = cur.UID
		_, err = tx.JournalWriter().PostJournal(ctx, core.JournalInput{
			JournalTypeUID: jtUID,
			IdempotencyKey: postgrestest.UniqueKey("dimcache-fresh"),
			Entries: []core.EntryInput{
				{AccountHolder: 1, CurrencyUID: cur.UID, ClassificationUID: userCls, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(7)},
				{AccountHolder: -1, CurrencyUID: cur.UID, ClassificationUID: systemCls, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(7)},
			},
		})
		return err
	}))

	bal, err := svc.BalanceReader().GetBalance(ctx, 1, freshUID, userCls)
	require.NoError(t, err)
	assert.True(t, bal.Equal(decimal.NewFromInt(7)), "got %s", bal)
}
