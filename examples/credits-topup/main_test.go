package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/presets"
)

// Exercise the public Go facade as ledger_app, with real migrations/triggers.
func fixture(t *testing.T) (*ledger.Service, *pgxpool.Pool, string, string) {
	t.Helper()
	admin := postgrestest.SetupDB(t)
	cfg := admin.Config().Copy()
	cfg.ConnConfig.RuntimeParams["role"] = "ledger_app"
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	usdc, credits, err := setup(t.Context(), svc)
	require.NoError(t, err)
	return svc, admin, usdc, credits
}

func balance(t *testing.T, svc *ledger.Service, currency, want, held string) {
	t.Helper()
	wallet, err := svc.Classifications().GetByCode(t.Context(), "main_wallet")
	require.NoError(t, err)
	got, err := svc.BalanceReader().GetBalance(t.Context(), userID, currency, wallet.UID)
	require.NoError(t, err)
	require.True(t, got.Equal(decimal.RequireFromString(want)), "balance = %s, want %s", got, want)
	hold, err := svc.Reserver().HeldAmount(t.Context(), userID, currency)
	require.NoError(t, err)
	require.True(t, hold.Equal(decimal.RequireFromString(held)), "held = %s, want %s", hold, held)
}

func deposit(t *testing.T, svc *ledger.Service, currency string) {
	t.Helper()
	_, err := svc.JournalWriter().ExecuteTemplate(t.Context(), "deposit_confirm", core.TemplateParams{
		HolderID: userID, CurrencyUID: currency, IdempotencyKey: "confirmed-chain-event",
		Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)},
	})
	require.NoError(t, err)
}

func reserve(t *testing.T, svc *ledger.Service, currency, key string, amount int64) *core.Reservation {
	t.Helper()
	r, err := svc.Reserver().Reserve(t.Context(), core.ReserveInput{
		AccountHolder: userID, CurrencyUID: currency, Amount: decimal.NewFromInt(amount),
		IdempotencyKey: key, ExpiresIn: time.Hour,
	})
	require.NoError(t, err)
	return r
}

func journalCount(t *testing.T, admin *pgxpool.Pool) int {
	t.Helper()
	var count int
	require.NoError(t, admin.QueryRow(t.Context(), "SELECT count(*) FROM journals").Scan(&count))
	return count
}

func TestCreditsScenario_RestartIsNoOp(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	require.NoError(t, scenario(ctx, svc, usdc, credits))
	balance(t, svc, credits, "912.875", "0")
	balance(t, svc, usdc, "0", "0")
	count := journalCount(t, admin)
	require.Equal(t, 7, count) // deposit + purchase pair + fixed + metered + two stream events
	require.NoError(t, scenario(ctx, svc, usdc, credits))
	require.Equal(t, count, journalCount(t, admin))
	require.NoError(t, checkFinalBalances(ctx, svc, usdc, credits))

	reconciled, err := svc.Reconciler().CheckAccountingEquation(ctx)
	require.NoError(t, err)
	require.True(t, reconciled.Balanced, "%+v", reconciled)
	_, err = svc.Templates().GetTemplate(ctx, "withdraw_confirm")
	require.ErrorIs(t, err, core.ErrNotFound)
}

func TestPurchase_RequiresFundsAndRollsBackBothCurrencies(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	err := purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase")
	require.ErrorIs(t, err, core.ErrInsufficientBalance)
	require.Zero(t, journalCount(t, admin))
	deposit(t, svc, usdc)

	// A failure resolving the second currency must undo the debit, reservation
	// creation and settlement, not leave an orphaned paid purchase.
	err = purchaseCredits(ctx, svc, usdc, "00000000-0000-0000-0000-000000000001", decimal.NewFromInt(1), "purchase")
	require.Error(t, err)
	balance(t, svc, usdc, "1", "0")
	balance(t, svc, credits, "0", "0")
	require.Equal(t, 1, journalCount(t, admin))

	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	balance(t, svc, usdc, "0", "0")
	balance(t, svc, credits, "1000", "0")
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	require.Equal(t, 3, journalCount(t, admin))
	require.ErrorIs(t, purchaseCredits(ctx, svc, usdc, credits, decimal.RequireFromString("0.5"), "purchase"), core.ErrConflict)
}

func TestUsage_ReplayOvershootAndFailedCharge(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	r := reserve(t, svc, credits, "job", 50)
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.NewFromInt(51), "usage", false), core.ErrInvalidInput)
	balance(t, svc, credits, "1000", "50")

	// Simulate an operationally frozen account after work was performed: a
	// rejected debit must keep the reservation, with no successful receipt.
	wallet, err := svc.Classifications().GetByCode(ctx, "main_wallet")
	require.NoError(t, err)
	policy := core.AccountPolicyInput{AccountHolder: userID, CurrencyUID: credits,
		ClassificationUID: wallet.UID, Status: core.AccountPolicyStatusFrozen, EnforceMinBalance: true}
	_, err = svc.AccountPolicies().SetPolicy(ctx, policy)
	require.NoError(t, err)
	require.Error(t, captureCredits(ctx, svc, r, decimal.NewFromInt(32), "usage", false))
	balance(t, svc, credits, "1000", "50")
	require.Equal(t, 3, journalCount(t, admin))
	policy.Status = core.AccountPolicyStatusActive
	_, err = svc.AccountPolicies().SetPolicy(ctx, policy)
	require.NoError(t, err)
	require.NoError(t, captureCredits(ctx, svc, r, decimal.NewFromInt(32), "usage", false))
	require.NoError(t, captureCredits(ctx, svc, r, decimal.NewFromInt(32), "usage", false))
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.NewFromInt(33), "usage", false), core.ErrConflict)
	balance(t, svc, credits, "968", "0")
	require.Equal(t, 4, journalCount(t, admin))
}

func TestStreamingUsage_IncrementalReplayAndCancellation(t *testing.T) {
	svc, _, usdc, credits := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	r := reserve(t, svc, credits, "stream", 100)
	for range 2 {
		require.NoError(t, captureCredits(ctx, svc, r, decimal.NewFromInt(30), "event-1", true))
	}
	balance(t, svc, credits, "970", "70")
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.NewFromInt(31), "event-1", true), core.ErrConflict)
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.NewFromInt(71), "event-2", true), core.ErrInvalidInput)
	// A cancelled stream keeps previously charged usage and releases only the
	// unconsumed budget. It does not refund the earlier 30-credit journal.
	require.NoError(t, svc.Reserver().Release(ctx, core.ReleaseInput{ReservationUID: r.UID, IdempotencyKey: "cancel"}))
	require.NoError(t, captureCredits(ctx, svc, r, decimal.NewFromInt(30), "event-1", true))
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.NewFromInt(1), "late-event", true), core.ErrInvalidTransition)
	balance(t, svc, credits, "970", "0")
}

func TestConcurrentBudgets_CannotOvercommitCredits(t *testing.T) {
	svc, _, usdc, credits := fixture(t)
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(t.Context(), svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, key := range []string{"job-a", "job-b"} {
		wg.Go(func() {
			<-start
			_, err := svc.Reserver().Reserve(t.Context(), core.ReserveInput{
				AccountHolder: userID, CurrencyUID: credits, Amount: decimal.NewFromInt(600),
				IdempotencyKey: key, ExpiresIn: time.Hour,
			})
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var succeeded int
	for err := range results {
		if err == nil {
			succeeded++
		} else {
			require.ErrorIs(t, err, core.ErrInsufficientBalance)
		}
	}
	require.Equal(t, 1, succeeded)
	balance(t, svc, credits, "1000", "600")
}

func TestCancelledContext_ReleaseUsesBoundedCleanupContext(t *testing.T) {
	svc, _, usdc, credits := fixture(t)
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(t.Context(), svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	r := reserve(t, svc, credits, "cancelled", 20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.Error(t, captureCredits(ctx, svc, r, decimal.Zero, "failed-result", false))
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	require.NoError(t, captureCredits(cleanup, svc, r, decimal.Zero, "failed-result", false))
	balance(t, svc, credits, "1000", "0")
}

func TestPurchase_PendingOrCancelledDepositCannotBuyCredits(t *testing.T) {
	svc, _, usdc, credits := fixture(t)
	ctx := t.Context()
	require.NoError(t, presets.InstallPendingBundle(ctx, svc.Classifications(), svc.JournalTypes(), svc.Templates()))
	_, err := svc.PendingBalanceWriter().AddPending(ctx, core.AddPendingInput{
		AccountHolder: userID, CurrencyUID: usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "pending-event",
	})
	require.NoError(t, err)
	require.ErrorIs(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"), core.ErrInsufficientBalance)
	_, err = svc.PendingBalanceWriter().CancelPending(ctx, core.CancelPendingInput{
		AccountHolder: userID, CurrencyUID: usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "cancel-event",
	})
	require.NoError(t, err)
	require.ErrorIs(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"), core.ErrInsufficientBalance)
	balance(t, svc, credits, "0", "0")

	_, err = svc.PendingBalanceWriter().AddPending(ctx, core.AddPendingInput{
		AccountHolder: userID, CurrencyUID: usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "second-pending-event",
	})
	require.NoError(t, err)
	_, err = svc.PendingBalanceWriter().ConfirmPending(ctx, core.ConfirmPendingInput{
		AccountHolder: userID, CurrencyUID: usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "confirmed-event",
	})
	require.NoError(t, err)
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	balance(t, svc, credits, "1000", "0")
}

func TestCredits_ExactPrecisionAndExpiredUsage(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	require.Error(t, purchaseCredits(ctx, svc, usdc, credits, decimal.RequireFromString("0.0000001"), "too-precise"))
	balance(t, svc, usdc, "1", "0")
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.RequireFromString("0.000001"), "micro-purchase"))
	balance(t, svc, usdc, "0.999999", "0")
	balance(t, svc, credits, "0.001", "0")
	r, err := svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: userID, CurrencyUID: credits, Amount: decimal.RequireFromString("0.001"),
		IdempotencyKey: "short-stream", ExpiresIn: time.Second,
	})
	require.NoError(t, err)
	require.NoError(t, captureCredits(ctx, svc, r, decimal.RequireFromString("0.000001"), "first-micro-event", true))
	// Observe the database's expiry clock, without mutating immutable timestamps.
	require.Eventually(t, func() bool {
		var expired bool
		err := admin.QueryRow(ctx, "SELECT expires_at <= clock_timestamp() FROM reservations WHERE uid = $1", r.UID).Scan(&expired)
		return err == nil && expired
	}, 5*time.Second, 20*time.Millisecond)
	require.ErrorIs(t, captureCredits(ctx, svc, r, decimal.RequireFromString("0.000001"), "late-micro-event", true), core.ErrInvalidTransition)
	// Replaying already-accounted usage remains valid after expiry, and final
	// closure frees the unused budget without another charge.
	require.NoError(t, captureCredits(ctx, svc, r, decimal.RequireFromString("0.000001"), "first-micro-event", true))
	require.NoError(t, svc.Reserver().FinalizeSettlement(ctx, core.FinalizeSettlementInput{ReservationUID: r.UID, IdempotencyKey: "close-stream"}))
	balance(t, svc, credits, "0.000999", "0")
}
