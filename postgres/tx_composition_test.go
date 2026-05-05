package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ledger "github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// TestTxComposition_Rollback verifies that a journal posted via a LedgerStore
// bound to a caller-owned transaction is NOT visible after that transaction is
// rolled back.
func TestTxComposition_Rollback(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-rb", "Tether USD (rollback)")
	jtID := postgrestest.SeedJournalType(t, pool, "txcomp-rb", "TxComp Rollback")
	clsWallet := postgrestest.SeedClassification(t, pool, "txcomp-wallet-rb", "TxComp Wallet rb", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "txcomp-custodial-rb", "TxComp Custodial rb", "credit", true)

	idemKey := postgrestest.UniqueKey("txcomp-rollback")

	// Begin a caller-owned transaction.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	// Bind LedgerStore to the caller's transaction.
	txStore := postgres.NewLedgerStore(pool).WithDB(tx)

	// Post a journal inside the caller's transaction.
	j, err := txStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: idemKey,
		Entries: []core.EntryInput{
			{AccountHolder: 10, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -10, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
		Source: "txcomp-test",
	})
	require.NoError(t, err)
	assert.True(t, j.ID > 0, "journal should have a positive ID within the open tx")

	// Roll back — the journal must disappear.
	require.NoError(t, tx.Rollback(ctx))

	// Verify via a fresh pool-mode store.
	poolStore := postgres.NewLedgerStore(pool)

	bal, err := poolStore.GetBalance(ctx, 10, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.IsZero(), "balance must be zero after rollback, got %s", bal)

	var journalCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM journals WHERE idempotency_key = $1", idemKey,
	).Scan(&journalCount))
	assert.Equal(t, 0, journalCount, "rolled-back journal must not persist")
}

// TestTxComposition_Commit verifies that a journal posted via a LedgerStore
// bound to a caller-owned transaction IS visible after that transaction commits.
func TestTxComposition_Commit(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	curID := postgrestest.SeedCurrency(t, pool, "USDT-cm", "Tether USD (commit)")
	jtID := postgrestest.SeedJournalType(t, pool, "txcomp-cm", "TxComp Commit")
	clsWallet := postgrestest.SeedClassification(t, pool, "txcomp-wallet-cm", "TxComp Wallet cm", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "txcomp-custodial-cm", "TxComp Custodial cm", "credit", true)

	idemKey := postgrestest.UniqueKey("txcomp-commit")
	amount := decimal.NewFromInt(250)

	// Begin a caller-owned transaction.
	tx, err := pool.Begin(ctx)
	require.NoError(t, err)

	// Bind LedgerStore to the caller's transaction.
	txStore := postgres.NewLedgerStore(pool).WithDB(tx)

	j, err := txStore.PostJournal(ctx, core.JournalInput{
		JournalTypeID:  jtID,
		IdempotencyKey: idemKey,
		Entries: []core.EntryInput{
			{AccountHolder: 20, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: amount},
			{AccountHolder: -20, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: amount},
		},
		Source: "txcomp-test",
	})
	require.NoError(t, err)
	assert.True(t, j.ID > 0)
	assert.True(t, j.TotalDebit.Equal(amount))

	// Commit — the journal must be durable.
	require.NoError(t, tx.Commit(ctx))

	// Verify via a fresh pool-mode store.
	poolStore := postgres.NewLedgerStore(pool)

	bal, err := poolStore.GetBalance(ctx, 20, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(amount), "expected %s after commit, got %s", amount, bal)

	var journalCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM journals WHERE idempotency_key = $1", idemKey,
	).Scan(&journalCount))
	assert.Equal(t, 1, journalCount, "committed journal must be visible")
}

// TestTxComposition_RunInTx verifies the high-level Service.RunInTx facade:
// - A callback returning nil commits the transaction.
// - A callback returning an error rolls the transaction back.
func TestTxComposition_RunInTx(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-runtx", "Tether USD (RunInTx)")
	jtID := postgrestest.SeedJournalType(t, pool, "txcomp-runtx", "TxComp RunInTx")
	clsWallet := postgrestest.SeedClassification(t, pool, "txcomp-wallet-runtx", "TxComp Wallet runtx", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "txcomp-custodial-runtx", "TxComp Custodial runtx", "credit", true)

	idemCommit := postgrestest.UniqueKey("runtx-commit")
	idemAbort := postgrestest.UniqueKey("runtx-abort")
	amount := decimal.NewFromInt(300)

	buildInput := func(key string) core.JournalInput {
		return core.JournalInput{
			JournalTypeID:  jtID,
			IdempotencyKey: key,
			Entries: []core.EntryInput{
				{AccountHolder: 30, CurrencyID: curID, ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, Amount: amount},
				{AccountHolder: -30, CurrencyID: curID, ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, Amount: amount},
			},
			Source: "runtx-test",
		}
	}

	// Case 1: successful commit.
	err = svc.RunInTx(ctx, func(txSvc *ledger.Service) error {
		_, err := txSvc.JournalWriter().PostJournal(ctx, buildInput(idemCommit))
		return err
	})
	require.NoError(t, err)

	bal, err := svc.BalanceReader().GetBalance(ctx, 30, curID, clsWallet)
	require.NoError(t, err)
	assert.True(t, bal.Equal(amount), "committed via RunInTx: expected %s got %s", amount, bal)

	// Case 2: rollback when fn returns error.
	err = svc.RunInTx(ctx, func(txSvc *ledger.Service) error {
		_, postErr := txSvc.JournalWriter().PostJournal(ctx, buildInput(idemAbort))
		if postErr != nil {
			return postErr
		}
		return errors.New("simulated business failure")
	})
	require.Error(t, err)

	var journalCount int
	require.NoError(t, pool.QueryRow(ctx,
		"SELECT COUNT(*) FROM journals WHERE idempotency_key = $1", idemAbort,
	).Scan(&journalCount))
	assert.Equal(t, 0, journalCount, "aborted RunInTx must not persist journal")
}

func TestTxComposition_RunInTx_BookingEventJournalLinkage(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	curID := postgrestest.SeedCurrency(t, pool, "USDT-link", "Tether USD (link)")
	jtID := postgrestest.SeedJournalType(t, pool, "booking-link", "Booking Link")

	lifecycle := &core.Lifecycle{
		Initial:  "pending",
		Terminal: []core.Status{"confirmed"},
		Transitions: map[core.Status][]core.Status{
			"pending": {"confirmed"},
		},
	}

	clsBooking, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code:       "booking_link_deposit",
		Name:       "Booking Link Deposit",
		NormalSide: core.NormalSideCredit,
		Lifecycle:  lifecycle,
	})
	require.NoError(t, err)

	clsWallet := postgrestest.SeedClassification(t, pool, "booking-link-wallet", "Booking Link Wallet", "debit", false)
	clsCustodial := postgrestest.SeedClassification(t, pool, "booking-link-custodial", "Booking Link Custodial", "credit", true)

	_, err = svc.Templates().CreateTemplate(ctx, core.TemplateInput{
		Code:          "booking_link_confirm",
		Name:          "Booking Link Confirm",
		JournalTypeID: jtID,
		Lines: []core.TemplateLineInput{
			{ClassificationID: clsWallet, EntryType: core.EntryTypeDebit, HolderRole: core.HolderRoleUser, AmountKey: "amount", SortOrder: 1},
			{ClassificationID: clsCustodial, EntryType: core.EntryTypeCredit, HolderRole: core.HolderRoleSystem, AmountKey: "amount", SortOrder: 2},
		},
	})
	require.NoError(t, err)

	booking, err := svc.Booker().CreateBooking(ctx, core.CreateBookingInput{
		ClassificationCode: clsBooking.Code,
		AccountHolder:      80,
		CurrencyID:         curID,
		Amount:             decimal.NewFromInt(125),
		IdempotencyKey:     postgrestest.UniqueKey("booking-link-create"),
		ChannelName:        "manual",
	})
	require.NoError(t, err)

	var eventID int64
	var journalID int64
	err = svc.RunInTx(ctx, func(txSvc *ledger.Service) error {
		evt, err := txSvc.Booker().Transition(ctx, core.TransitionInput{
			BookingID: booking.ID,
			ToStatus:  "confirmed",
			Amount:    decimal.NewFromInt(125),
			ActorID:   80,
			Source:    "tx-test",
		})
		if err != nil {
			return err
		}
		eventID = evt.ID

		j, err := txSvc.JournalWriter().ExecuteTemplate(ctx, "booking_link_confirm", core.TemplateParams{
			HolderID:       80,
			CurrencyID:     curID,
			IdempotencyKey: postgrestest.UniqueKey("booking-link-journal"),
			EventID:        evt.ID,
			Amounts:        map[string]decimal.Decimal{"amount": decimal.NewFromInt(125)},
			ActorID:        80,
			Source:         "tx-test",
		})
		if err != nil {
			return err
		}
		journalID = j.ID
		return nil
	})
	require.NoError(t, err)
	require.NotZero(t, eventID)
	require.NotZero(t, journalID)

	trace, err := svc.Audit().TraceBooking(ctx, booking.ID)
	require.NoError(t, err)
	require.Len(t, trace.Events, 1)
	require.Len(t, trace.Journals, 1)
	require.NotNil(t, trace.Booking.JournalID)
	require.NotNil(t, trace.Events[0].JournalID)
	assert.Equal(t, journalID, *trace.Booking.JournalID)
	assert.Equal(t, journalID, *trace.Events[0].JournalID)
	assert.Equal(t, journalID, trace.Journals[0].ID)
}
