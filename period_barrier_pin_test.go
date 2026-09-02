package ledger_test

// Pins for I-59 (the period-close barrier), driven from the real consumption
// entry point: ledger.New(pool) + Service.RunInTx, the composition a library
// consumer actually writes. The pre-fix bug was invisible to every existing
// I-15 pin because all six of them are single-threaded ("close, then post,
// assert rejected") and the hole was purely a matter of two transactions'
// relative timing.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

type barrierDims struct {
	currencyUID    string
	journalTypeUID string
	userClsUID     string
	systemClsUID   string
}

func seedBarrierDims(t *testing.T, pool *pgxpool.Pool) barrierDims {
	t.Helper()
	return barrierDims{
		currencyUID:    postgrestest.SeedCurrency(t, pool, "USDT", "Tether USD"),
		journalTypeUID: postgrestest.SeedJournalType(t, pool, "transfer", "Transfer"),
		userClsUID:     postgrestest.SeedClassification(t, pool, "main_wallet", "Wallet", "debit", false),
		systemClsUID:   postgrestest.SeedClassification(t, pool, "custodial", "Custodial", "credit", true),
	}
}

func (d barrierDims) journal(key string, effectiveAt time.Time) core.JournalInput {
	return core.JournalInput{
		JournalTypeUID: d.journalTypeUID,
		IdempotencyKey: key,
		EffectiveAt:    effectiveAt,
		Entries: []core.EntryInput{
			{AccountHolder: 1, CurrencyUID: d.currencyUID, ClassificationUID: d.userClsUID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(10)},
			{AccountHolder: -1, CurrencyUID: d.currencyUID, ClassificationUID: d.systemClsUID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(10)},
		},
	}
}

// TestClosePeriod_WaitsForInFlightBackdatedJournal is the I-59 concurrency
// pin. A consumer's RunInTx posts a backdated journal and then keeps its
// transaction open (exactly what RunInTx exists for — composing the caller's
// own writes); an operator closes the period covering that date at the same
// moment.
//
// Post-fix the close blocks until the writer's transaction resolves, so the
// journal is either rejected (had the close won the read) or predates the
// line becoming active. Pre-fix ClosePeriod took no lock whatsoever and
// returned in milliseconds while the writer was still in flight — asserted
// here as an ordering property (close returns after the writer commits),
// which is the observable that actually distinguishes the two.
func TestClosePeriod_WaitsForInFlightBackdatedJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	dims := seedBarrierDims(t, pool)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	closeBefore := time.Now().Truncate(time.Microsecond).AddDate(0, 0, -5)
	backdated := closeBefore.AddDate(0, 0, -1)

	posted := make(chan struct{})
	type closeResult struct {
		at  time.Time
		err error
	}
	closed := make(chan closeResult, 1)

	go func() {
		<-posted
		_, err := svc.PeriodCloser().ClosePeriod(ctx, core.ClosePeriodInput{
			CloseBefore: closeBefore,
			Note:        "month-end close racing an in-flight backdated journal",
			ActorID:     1,
		})
		closed <- closeResult{at: time.Now(), err: err}
	}()

	var committedAt time.Time
	txErr := svc.RunInTx(ctx, func(tx *ledger.Service) error {
		if _, err := tx.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("barrier-inflight"), backdated)); err != nil {
			return err
		}
		close(posted)
		// Stand in for a consumer callback doing its own work. The pre-fix
		// race needed nothing more than this: ClosePeriod committed inside
		// this window.
		time.Sleep(1500 * time.Millisecond)
		committedAt = time.Now()
		return nil
	})

	res := <-closed

	if txErr != nil {
		// The other legal outcome: the close won the read and the journal was
		// refused. Either way the two must not both succeed with the journal
		// behind the line.
		require.ErrorIs(t, txErr, core.ErrPeriodClosed)
		require.NoError(t, res.err)
		return
	}
	require.NoError(t, res.err, "close period must not fail once the writer commits")
	require.False(t, committedAt.IsZero())
	assert.True(t, res.at.After(committedAt),
		"ClosePeriod returned at %s, before the in-flight backdated journal committed at %s: the close line became active while a writer that had already passed the gate was still in flight (I-59)",
		res.at.Format(time.RFC3339Nano), committedAt.Format(time.RFC3339Nano))
}

// TestClosePeriod_RejectsAfterBarrier is the ordinary-path companion: with
// the barrier in place a close still succeeds promptly when nothing is in
// flight, and still rejects a later backdated posting.
func TestClosePeriod_RejectsAfterBarrier(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	dims := seedBarrierDims(t, pool)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	closeBefore := time.Now().Truncate(time.Microsecond).AddDate(0, 0, -5)
	_, err = svc.PeriodCloser().ClosePeriod(ctx, core.ClosePeriodInput{CloseBefore: closeBefore, ActorID: 1})
	require.NoError(t, err)

	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("barrier-after"), closeBefore.AddDate(0, 0, -1)))
	require.ErrorIs(t, err, core.ErrPeriodClosed)

	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("barrier-open"), time.Time{}))
	require.NoError(t, err)
}

// TestReconcile_PeriodCloseViolations_ReportsForgedBackdatedJournal pins the
// independent observable half of I-59: a barrier that nothing can falsify is
// an assertion, not a control. The suite runs through the real facade entry
// (svc.FullReconciler), and the violating row is forged with a raw INSERT --
// which is exactly the class this check exists to catch (a journal that
// reached the table without passing the gate).
func TestReconcile_PeriodCloseViolations_ReportsForgedBackdatedJournal(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	dims := seedBarrierDims(t, pool)
	svc, err := ledger.New(pool)
	require.NoError(t, err)
	ctx := context.Background()

	// A legitimate journal in the open period, then a close line after it.
	_, err = svc.JournalWriter().PostJournal(ctx, dims.journal(postgrestest.UniqueKey("violation-seed"), time.Now().AddDate(0, 0, -10)))
	require.NoError(t, err)

	line, err := svc.PeriodCloser().ClosePeriod(ctx, core.ClosePeriodInput{
		CloseBefore: time.Now().Truncate(time.Microsecond).AddDate(0, 0, -5),
		ActorID:     1,
	})
	require.NoError(t, err)

	full := svc.FullReconciler(service.FullReconciliationConfig{})

	// Clean baseline: the seed journal predates the line, which is the normal
	// state of every closed period and must NOT be reported.
	report, err := full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	require.True(t, findCheck(t, report, "period_close_violations").Passed,
		"a journal that existed before the close line was committed is not a violation")

	// Forge one: same shape as the seed journal, but written after the line.
	_, err = pool.Exec(ctx, `
		INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit,
		                      metadata, actor_id, source, effective_at, created_at, uid,
		                      auth_digest, auth_signature, auth_key_id, auth_status)
		SELECT journal_type_id, 'forged-' || idempotency_key, total_debit, total_credit,
		       metadata, actor_id, source, $1, $2, gen_random_uuid(),
		       auth_digest, auth_signature, auth_key_id, auth_status
		FROM journals ORDER BY id LIMIT 1`,
		line.CloseBefore.AddDate(0, 0, -1), line.CreatedAt.Add(time.Second),
	)
	require.NoError(t, err)

	report, err = full.RunFullReconciliation(ctx)
	require.NoError(t, err)
	check := findCheck(t, report, "period_close_violations")
	assert.False(t, check.Passed, "the forged backdated journal must be reported")
	assert.NotEmpty(t, check.Findings)
}

func findCheck(t *testing.T, report *core.ReconcileReport, name string) core.CheckResult {
	t.Helper()
	for _, c := range report.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("check %q is not registered in the full reconciliation suite", name)
	return core.CheckResult{}
}
