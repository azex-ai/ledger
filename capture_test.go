package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
)

func reserveForCapture(t *testing.T, ctx context.Context, f exchangeFixture) *core.Reservation {
	t.Helper()
	r, err := f.svc.Reserver().Reserve(ctx, core.ReserveInput{
		AccountHolder: f.holder, CurrencyUID: f.usdc, Amount: decimal.NewFromInt(1),
		IdempotencyKey: "capture-reserve", ExpiresIn: time.Minute,
	})
	require.NoError(t, err)
	return r
}

func captureInput(r *core.Reservation, key, amount string, partial bool) ledger.CaptureInput {
	return ledger.CaptureInput{ReservationUID: r.UID, IdempotencyKey: key,
		TemplateCode: "fx_sell", Amount: decimal.RequireFromString(amount), Partial: partial,
		ActorID: 42, Source: "capture-test", Metadata: map[string]string{"event_id": key}}
}

func TestCapture_FullAndReplayBindTheCharge(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	in := captureInput(r, "full", "0.6", false)
	result, err := f.svc.Capture(ctx, in)
	require.NoError(t, err)
	require.Equal(t, r.UID, result.ReservationUID)
	require.Equal(t, map[string]string{"event_id": "full"}, in.Metadata, "caller metadata must not be mutated")
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.4")))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
	stored, err := f.svc.ReservationReader().GetReservation(ctx, r.UID)
	require.NoError(t, err)
	require.Equal(t, core.ReservationStatusSettled, stored.Status)
	require.NotNil(t, stored.SettledAmount)
	require.True(t, stored.SettledAmount.Equal(in.Amount))
	j, entries, err := f.svc.Queries().GetJournal(ctx, result.JournalUID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.True(t, j.TotalDebit.Equal(j.TotalCredit))
	require.Equal(t, "full:charge", j.IdempotencyKey)
	require.Equal(t, r.UID, j.Metadata["reservation_uid"])
	require.Equal(t, "full", j.Metadata["capture_mode"])
	require.Equal(t, "fx_sell", j.Metadata["capture_template_code"])
	require.Equal(t, core.AuthStatusUnsignedTxMode, j.AuthStatus)
	require.Equal(t, int64(42), j.ActorID)

	replayed, err := f.svc.Capture(ctx, in)
	require.NoError(t, err)
	require.Equal(t, result, replayed)
	f.installTemplate(t, ctx, "capture-alias", "fx_sell", core.EntryTypeCredit)
	for _, edit := range []func(*ledger.CaptureInput){
		func(v *ledger.CaptureInput) { v.Amount = decimal.RequireFromString("0.5") },
		func(v *ledger.CaptureInput) { v.Source = "changed-source" },
		func(v *ledger.CaptureInput) { v.ActorID++ },
		func(v *ledger.CaptureInput) { v.TemplateCode = "capture-alias" },
		func(v *ledger.CaptureInput) { v.Metadata = map[string]string{"event_id": "different"} },
	} {
		changed := in
		edit(&changed)
		_, err := f.svc.Capture(ctx, changed)
		require.ErrorIs(t, err, core.ErrConflict)
	}
	changedMode := in
	changedMode.Partial = true
	_, err = f.svc.Capture(ctx, changedMode)
	require.ErrorIs(t, err, core.ErrInvalidTransition, "mode change can be rejected by settlement before journal replay")
	require.Equal(t, 2, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.4")))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
}

func TestCapture_PartialTracksOnlyIncrementalSpend(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	first := captureInput(r, "partial-first", "0.3", true)
	result, err := f.svc.Capture(ctx, first)
	require.NoError(t, err)
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.7")))
	require.True(t, f.held(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.7")))
	wrongMode := first
	wrongMode.Partial = false
	_, err = f.svc.Capture(ctx, wrongMode)
	require.ErrorIs(t, err, core.ErrInvalidTransition)
	require.Equal(t, 2, f.journalCount(t, ctx))
	require.True(t, f.held(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.7")))
	_, err = f.svc.Capture(ctx, captureInput(r, "partial-second", "0.4", true))
	require.NoError(t, err)
	_, err = f.svc.Capture(ctx, captureInput(r, "too-much", "0.4", true))
	require.ErrorIs(t, err, core.ErrInvalidInput)
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.3")))
	require.True(t, f.held(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.3")))
	require.Equal(t, 3, f.journalCount(t, ctx))
	require.NoError(t, f.svc.Reserver().FinalizeSettlement(ctx, core.FinalizeSettlementInput{ReservationUID: r.UID, IdempotencyKey: "partial-final"}))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
	replayed, err := f.svc.Capture(ctx, first)
	require.NoError(t, err)
	require.Equal(t, result, replayed, "an already applied increment remains replayable after finalization")
	require.Equal(t, 3, f.journalCount(t, ctx))
}

func TestCapture_RefusesUnsafeTemplatesAndRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, code := range []string{"does-not-exist", "fx_buy", "capture-pending", "fee_charge", "capture-double-charge"} {
		t.Run(code, func(t *testing.T) {
			f := seedExchangeFixture(t, ctx)
			r := reserveForCapture(t, ctx, f)
			if code == "capture-pending" || code == "capture-double-charge" {
				tmpl, err := f.svc.Templates().GetTemplate(ctx, "fx_sell")
				require.NoError(t, err)
				lines := make([]core.TemplateLineInput, 0, 4)
				for _, line := range tmpl.Lines {
					if code == "capture-pending" && line.HolderRole == core.HolderRoleUser {
						pending, err := f.svc.Classifications().GetByCode(ctx, "pending")
						require.NoError(t, err)
						line.ClassificationUID = pending.UID
					}
					lines = append(lines, core.TemplateLineInput{ClassificationUID: line.ClassificationUID, EntryType: line.EntryType, HolderRole: line.HolderRole, AmountKey: "amount", SortOrder: line.SortOrder})
				}
				if code == "capture-double-charge" {
					for _, line := range append([]core.TemplateLineInput(nil), lines...) {
						line.SortOrder += 2
						lines = append(lines, line)
					}
				}
				_, err = f.svc.Templates().CreateTemplate(ctx, core.TemplateInput{Code: code, Name: code, JournalTypeUID: tmpl.JournalTypeUID, Lines: lines})
				require.NoError(t, err)
			}
			in := captureInput(r, "unsafe", "0.2", false)
			in.TemplateCode = code
			_, err := f.svc.Capture(ctx, in)
			require.Error(t, err)
			if code != "does-not-exist" {
				require.ErrorIs(t, err, core.ErrInvalidInput)
			}
			requireCaptureUntouched(t, ctx, f, r.UID)
		})
	}
}

func requireCaptureUntouched(t *testing.T, ctx context.Context, f exchangeFixture, uid string) {
	t.Helper()
	require.Equal(t, 1, f.journalCount(t, ctx), "failed capture cannot leave a charge journal")
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
	require.True(t, f.held(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
	r, err := f.svc.ReservationReader().GetReservation(ctx, uid)
	require.NoError(t, err)
	require.Equal(t, core.ReservationStatusActive, r.Status)
	var receipts int
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM reservation_operation_receipts").Scan(&receipts))
	require.Zero(t, receipts, "failed journal must roll back settlement receipt")
}

func TestCapture_JoinsCallerTransactionAndUsesItsReservationReader(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	rollback := errors.New("caller rollback")
	var uid string
	err := f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		// The caller owns the lock union when composing Reserve + Capture.
		if err := tx.LockForTemplates(ctx, []core.TemplateExecutionRequest{{TemplateCode: "fx_sell", Params: core.TemplateParams{
			HolderID: f.holder, CurrencyUID: f.usdc, IdempotencyKey: "tx-capture:charge",
			Amounts: map[string]decimal.Decimal{"amount": decimal.RequireFromString("0.5")},
		}}}, "tx-reserve", "tx-capture:settle"); err != nil {
			return err
		}
		r, err := tx.Reserver().Reserve(ctx, core.ReserveInput{AccountHolder: f.holder, CurrencyUID: f.usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "tx-reserve"})
		if err != nil {
			return err
		}
		uid = r.UID
		if _, err := tx.Capture(ctx, captureInput(r, "tx-capture", "0.5", false)); err != nil {
			return err
		}
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	_, err = f.svc.ReservationReader().GetReservation(ctx, uid)
	require.ErrorIs(t, err, core.ErrNotFound)
	require.Equal(t, 1, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())

	r := reserveForCapture(t, ctx, f)
	err = f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		in := captureInput(r, "tx-bad", "0.5", false)
		in.TemplateCode = "fx_buy"
		_, err := tx.Capture(ctx, in)
		return err // Returning this error is required; Capture creates no savepoint.
	})
	require.ErrorIs(t, err, core.ErrInvalidInput)
	requireCaptureUntouched(t, ctx, f, r.UID)
}

func TestCapture_ConcurrentSameKeyAndDeposit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	in := captureInput(r, "concurrent", "0.5", false)
	start := make(chan struct{})
	results := make(chan string, 6)
	errs := make(chan error, 7)
	var wg sync.WaitGroup
	for range 6 {
		wg.Go(func() {
			<-start
			res, err := f.svc.Capture(ctx, in)
			errs <- err
			if err == nil {
				results <- res.JournalUID
			}
		})
	}
	wg.Go(func() {
		<-start
		_, err := f.svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{HolderID: f.holder, CurrencyUID: f.usdc, IdempotencyKey: "capture-concurrent-deposit", Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)}})
		errs <- err
	})
	close(start)
	wg.Wait()
	close(errs)
	close(results)
	for err := range errs {
		require.NoError(t, err)
	}
	var uid string
	for result := range results {
		if uid == "" {
			uid = result
		}
		require.Equal(t, uid, result)
	}
	require.NotEmpty(t, uid)
	require.Equal(t, 3, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("1.5")))
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
}

func TestCapture_PrelocksBothOperationKeys(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	require.NoError(t, f.svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if _, err := tx.Capture(ctx, captureInput(r, "prelock", "0.5", false)); err != nil {
			return err
		}
		for _, key := range []string{"prelock:settle", "prelock:charge"} {
			var held bool
			if err := tx.DBTX().QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
				WHERE locktype='advisory' AND pid=pg_backend_pid() AND objsubid=1
				AND ((classid::bigint << 32) | objid::bigint) = hashtextextended('idem:' || $1::text, 0))`, key).Scan(&held); err != nil {
				return err
			}
			if !held {
				return fmt.Errorf("key %s not prelocked", key)
			}
		}
		return nil
	}))
}

func TestCapture_RejectsExcessPrecisionAmountAndDifferentReservation(t *testing.T) {
	ctx := context.Background()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	for _, amount := range []string{"2", "0.0000001"} {
		_, err := f.svc.Capture(ctx, captureInput(r, "bad-amount", amount, false))
		require.Error(t, err)
		requireCaptureUntouched(t, ctx, f, r.UID)
	}
	_, err := f.svc.Capture(ctx, captureInput(r, "reuse", "0.2", false))
	require.NoError(t, err)
	other, err := f.svc.Reserver().Reserve(ctx, core.ReserveInput{AccountHolder: f.holder, CurrencyUID: f.usdc, Amount: decimal.RequireFromString("0.5"), IdempotencyKey: "second-reserve"})
	require.NoError(t, err)
	_, err = f.svc.Capture(ctx, captureInput(other, "reuse", "0.2", false))
	require.ErrorIs(t, err, core.ErrConflict)
	stored, err := f.svc.ReservationReader().GetReservation(ctx, other.UID)
	require.NoError(t, err)
	require.Equal(t, core.ReservationStatusActive, stored.Status)
	require.Equal(t, 2, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.8")))
	require.True(t, f.held(t, ctx, f.usdc).Equal(decimal.RequireFromString("0.5")))
}

func TestCapture_ReleasedAndExpiredReservationsCannotCharge(t *testing.T) {
	ctx := context.Background()
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%t", expired), func(t *testing.T) {
			f := seedExchangeFixture(t, ctx)
			ttl := time.Minute
			if expired {
				ttl = time.Millisecond
			}
			r, err := f.svc.Reserver().Reserve(ctx, core.ReserveInput{AccountHolder: f.holder, CurrencyUID: f.usdc, Amount: decimal.NewFromInt(1), IdempotencyKey: "expiry-reserve", ExpiresIn: ttl})
			require.NoError(t, err)
			if expired {
				require.Eventually(t, func() bool {
					var done bool
					err := f.pool.QueryRow(ctx, "SELECT clock_timestamp() >= expires_at FROM reservations WHERE uid=$1", r.UID).Scan(&done)
					return err == nil && done
				}, time.Second, time.Millisecond)
			} else {
				require.NoError(t, f.svc.Reserver().Release(ctx, core.ReleaseInput{ReservationUID: r.UID, IdempotencyKey: "released"}))
			}
			for _, partial := range []bool{false, true} {
				_, err := f.svc.Capture(ctx, captureInput(r, "not-live", "0.2", partial))
				require.ErrorIs(t, err, core.ErrInvalidTransition)
			}
			require.Equal(t, 1, f.journalCount(t, ctx))
			require.True(t, f.balance(t, ctx, f.usdc).Equal(decimal.NewFromInt(1)))
		})
	}
}

func TestCapture_ConcurrentPartialCallsCannotExceedReservation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	f := seedExchangeFixture(t, ctx)
	r := reserveForCapture(t, ctx, f)
	start := make(chan struct{})
	errs := make(chan error, 6)
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			<-start
			_, err := f.svc.Capture(ctx, captureInput(r, fmt.Sprintf("increment-%d", i), "0.2", true))
			errs <- err
		})
	}
	close(start)
	wg.Wait()
	close(errs)
	var applied, rejected int
	for err := range errs {
		if err == nil {
			applied++
		} else {
			require.ErrorIs(t, err, core.ErrInvalidInput)
			rejected++
		}
	}
	require.Equal(t, 5, applied)
	require.Equal(t, 1, rejected)
	require.Equal(t, 6, f.journalCount(t, ctx))
	require.True(t, f.balance(t, ctx, f.usdc).IsZero())
	require.True(t, f.held(t, ctx, f.usdc).IsZero())
	stored, err := f.svc.ReservationReader().GetReservation(ctx, r.UID)
	require.NoError(t, err)
	require.NotNil(t, stored.SettledAmount)
	require.True(t, stored.SettledAmount.Equal(decimal.NewFromInt(1)))
	var legs int
	require.NoError(t, f.pool.QueryRow(ctx, "SELECT count(*) FROM reservation_settlement_legs").Scan(&legs))
	require.Equal(t, 5, legs, "rejected charge must not accumulate a settlement leg")
}

func TestCapture_RejectsInvalidInputBeforeAccessingStores(t *testing.T) {
	svc := new(ledger.Service)
	valid := ledger.CaptureInput{ReservationUID: "r", TemplateCode: "t", IdempotencyKey: "k", Amount: decimal.NewFromInt(1)}
	for _, edit := range []func(*ledger.CaptureInput){
		func(v *ledger.CaptureInput) { v.ReservationUID = "" },
		func(v *ledger.CaptureInput) { v.TemplateCode = "" },
		func(v *ledger.CaptureInput) { v.IdempotencyKey = "" },
		func(v *ledger.CaptureInput) { v.Amount = decimal.Zero },
		func(v *ledger.CaptureInput) { v.Amount = decimal.NewFromInt(-1) },
		func(v *ledger.CaptureInput) { v.Amount = decimal.New(1, math.MaxInt32) },
		func(v *ledger.CaptureInput) { v.Amount = decimal.New(1, math.MinInt32) },
		func(v *ledger.CaptureInput) { v.Metadata = map[string]string{"reservation_uid": "r"} },
		func(v *ledger.CaptureInput) { v.Metadata = map[string]string{"capture_mode": "full"} },
		func(v *ledger.CaptureInput) { v.Metadata = map[string]string{"capture_template_code": "t"} },
	} {
		in := valid
		edit(&in)
		_, err := svc.Capture(context.Background(), in)
		require.ErrorIs(t, err, core.ErrInvalidInput)
	}
}
