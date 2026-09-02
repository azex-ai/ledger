package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"github.com/azex-ai/ledger/service"
)

// Compile-time interface assertions.
var (
	_ service.RollupQueuer             = (*RollupAdapter)(nil)
	_ service.CheckpointReadWriter     = (*RollupAdapter)(nil)
	_ service.EntrySummer              = (*RollupAdapter)(nil)
	_ service.GlobalSummer             = (*RollupAdapter)(nil)
	_ service.AccountEntrySummer       = (*RollupAdapter)(nil)
	_ service.CheckpointReader         = (*RollupAdapter)(nil)
	_ service.HistoricalBalanceLister  = (*RollupAdapter)(nil)
	_ service.SnapshotWriter           = (*RollupAdapter)(nil)
	_ service.CheckpointAggregator     = (*RollupAdapter)(nil)
	_ service.SystemRollupWriter       = (*RollupAdapter)(nil)
	_ service.ExpiredReservationFinder = (*RollupAdapter)(nil)
)

// RollupAdapter implements all service-layer store interfaces needed for background services.
type RollupAdapter struct {
	pool       *pgxpool.Pool
	q          *sqlcgen.Queries
	claimLease time.Duration
	dims       *dimCache
}

const rollupClaimLease = 2 * time.Minute

// NewRollupAdapter creates a new RollupAdapter.
func NewRollupAdapter(pool *pgxpool.Pool) *RollupAdapter {
	return &RollupAdapter{
		pool:       pool,
		q:          sqlcgen.New(pool),
		claimLease: rollupClaimLease,
		dims:       dimCacheFor(pool),
	}
}

// WithDB returns a transaction-bound clone. Pool is deliberately nil so
// methods use the caller's already-established transaction snapshot.
func (a *RollupAdapter) WithDB(db DBTX) *RollupAdapter {
	return &RollupAdapter{q: sqlcgen.New(db), claimLease: a.claimLease, dims: a.dims}
}

// SetClaimLease overrides the default rollup claim lease duration.
func (a *RollupAdapter) SetClaimLease(d time.Duration) {
	if d > 0 {
		a.claimLease = d
	}
}

// --- RollupQueuer ---

func (a *RollupAdapter) DequeueRollupBatch(ctx context.Context, batchSize int) ([]service.RollupQueueItem, error) {
	rows, err := a.q.DequeueRollupBatch(ctx, sqlcgen.DequeueRollupBatchParams{
		Limit:        int32(batchSize),
		ClaimedUntil: timeToTimestamptz(time.Now().Add(a.claimLease)),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: dequeue rollup batch: %w", err)
	}

	items := make([]service.RollupQueueItem, 0, batchSize)
	for _, row := range rows {
		item := service.RollupQueueItem{
			ID:               row.ID,
			AccountHolder:    row.AccountHolder,
			CurrencyID:       row.CurrencyID,
			ClassificationID: row.ClassificationID,
			CreatedAt:        row.CreatedAt,
			ClaimedUntil:     row.ClaimedUntil.Time,
		}
		items = append(items, item)
	}

	return items, nil
}

// MarkRollupProcessed marks the item processed only if claimToken still matches
// the row's claim (i.e. this worker still owns it). Returns false without error
// when the claim was lost to a concurrent re-dirty or re-claim — the row stays
// pending for its rightful owner.
func (a *RollupAdapter) MarkRollupProcessed(ctx context.Context, id int64, claimToken time.Time) (bool, error) {
	rows, err := a.q.MarkRollupProcessed(ctx, sqlcgen.MarkRollupProcessedParams{
		ID:           id,
		ClaimedUntil: timeToTimestamptz(claimToken),
	})
	if err != nil {
		return false, fmt.Errorf("postgres: mark rollup processed: %w", err)
	}
	return rows > 0, nil
}

// ReleaseRollupClaim releases the claim and bumps failed_attempts, but only if
// claimToken still matches — a stale worker must not penalize work it no longer
// owns.
func (a *RollupAdapter) ReleaseRollupClaim(ctx context.Context, id int64, claimToken time.Time) error {
	if err := a.q.ReleaseRollupClaim(ctx, sqlcgen.ReleaseRollupClaimParams{
		ID:           id,
		ClaimedUntil: timeToTimestamptz(claimToken),
	}); err != nil {
		return fmt.Errorf("postgres: release rollup claim: %w", err)
	}
	return nil
}

func (a *RollupAdapter) CountPendingRollups(ctx context.Context) (int64, error) {
	return a.q.CountPendingRollups(ctx)
}

// EnqueueRollup inserts a pending rollup for the dimension. If an unprocessed
// row already exists it re-dirties it (ON CONFLICT DO UPDATE SET claimed_until =
// NULL), so an enqueue landing while a worker is mid-processing forces a
// reprocess (paired with MarkRollupProcessed's claim guard) rather than being
// silently coalesced away. Used by the journal-posting path and by the rollup
// worker's post-processing re-enqueue (see RollupService.processItem).
func (a *RollupAdapter) EnqueueRollup(ctx context.Context, holder, currencyID, classificationID int64) error {
	if err := a.q.EnqueueRollup(ctx, sqlcgen.EnqueueRollupParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	}); err != nil {
		return fmt.Errorf("postgres: enqueue rollup: %w", err)
	}
	return nil
}

// --- CheckpointReadWriter ---

func (a *RollupAdapter) GetCheckpoint(ctx context.Context, holder, currencyID, classificationID int64) (*service.BalanceCheckpoint, error) {
	row, err := a.q.GetBalanceCheckpoint(ctx, sqlcgen.GetBalanceCheckpointParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: get checkpoint: %w", err)
	}
	return &service.BalanceCheckpoint{
		AccountHolder:    row.AccountHolder,
		CurrencyID:       row.CurrencyID,
		ClassificationID: row.ClassificationID,
		Balance:          mustNumericToDecimal(row.Balance),
		LastEntryID:      row.LastEntryID,
		LastEntryAt:      row.LastEntryAt,
		UpdatedAt:        row.UpdatedAt,
	}, nil
}

func (a *RollupAdapter) UpsertCheckpoint(ctx context.Context, cp service.BalanceCheckpoint) error {
	return a.q.UpsertBalanceCheckpoint(ctx, sqlcgen.UpsertBalanceCheckpointParams{
		AccountHolder:    cp.AccountHolder,
		CurrencyID:       cp.CurrencyID,
		ClassificationID: cp.ClassificationID,
		Balance:          decimalToNumeric(cp.Balance),
		LastEntryID:      cp.LastEntryID,
		LastEntryAt:      cp.LastEntryAt,
	})
}

// --- EntrySummer ---

func (a *RollupAdapter) SumEntriesSince(ctx context.Context, holder, currencyID, sinceEntryID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, maxEntryID int64, maxEntryAt time.Time, err error) {
	if a.pool == nil {
		// Tx mode (defensive — RollupAdapter is normally pool-backed): the
		// caller's transaction already provides the snapshot.
		return a.sumEntriesSinceWithQueries(ctx, a.q, holder, currencyID, sinceEntryID)
	}

	// Pool mode: the entry-sum read and the max-entry-id read MUST observe the
	// same snapshot. Without this, a journal committing between the two queries
	// is seen by the MAX (advancing the checkpoint's last_entry_id) but missed
	// by the SUM, so the checkpoint advances past an entry whose amount was
	// never counted — a permanent, silent balance under-count (GetBalance only
	// sums entries with id > last_entry_id). REPEATABLE READ pins one snapshot
	// across both reads, closing that window.
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	return a.sumEntriesSinceWithQueries(ctx, a.q.WithTx(tx), holder, currencyID, sinceEntryID)
}

// sumEntriesSinceWithQueries runs the two rollup reads (entry sums + max entry
// id) against the supplied queries handle. Both reads must run on the same
// handle so that, in pool mode, the caller's REPEATABLE READ transaction makes
// them observe a single snapshot. See SumEntriesSince for why this matters.
func (a *RollupAdapter) sumEntriesSinceWithQueries(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, sinceEntryID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, maxEntryID int64, maxEntryAt time.Time, err error) {
	rows, err := q.SumEntriesSinceCheckpoint(ctx, sqlcgen.SumEntriesSinceCheckpointParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		SinceEntryID:  sinceEntryID,
	})
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: %w", err)
	}

	debitByClass = make(map[int64]decimal.Decimal)
	creditByClass = make(map[int64]decimal.Decimal)

	for _, r := range rows {
		amount, err := anyToDecimal(r.Total)
		if err != nil {
			return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: convert: %w", err)
		}
		switch core.EntryType(r.EntryType) {
		case core.EntryTypeDebit:
			debitByClass[r.ClassificationID] = debitByClass[r.ClassificationID].Add(amount)
		case core.EntryTypeCredit:
			creditByClass[r.ClassificationID] = creditByClass[r.ClassificationID].Add(amount)
		}
	}

	maxRow, err := q.GetMaxEntryForAccountCurrencySince(ctx, sqlcgen.GetMaxEntryForAccountCurrencySinceParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		ID:            sinceEntryID,
	})
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: max entry: %w", err)
	}
	maxEntryID = maxRow.MaxEntryID
	maxEntryAt, err = anyToTime(maxRow.MaxEntryAt)
	if err != nil {
		return nil, nil, 0, time.Time{}, fmt.Errorf("postgres: sum entries since: max entry convert: %w", err)
	}
	if maxEntryID == 0 {
		maxEntryAt = time.Time{}
	}

	return debitByClass, creditByClass, maxEntryID, maxEntryAt, nil
}

// --- GlobalSummer ---

func (a *RollupAdapter) SumGlobalDebitCreditByCurrency(ctx context.Context) ([]service.CurrencyReconcileTotals, error) {
	rows, err := a.q.SumGlobalDebitCreditByCurrency(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: sum global by currency: %w", err)
	}

	totalsByCurrency := make(map[int64]service.CurrencyReconcileTotals)
	for _, row := range rows {
		amount, err := numericToDecimal(row.Total)
		if err != nil {
			return nil, fmt.Errorf("postgres: sum global by currency: convert: %w", err)
		}

		current := totalsByCurrency[row.CurrencyID]
		current.CurrencyID = row.CurrencyID
		switch core.EntryType(row.EntryType) {
		case core.EntryTypeDebit:
			current.Debit = current.Debit.Add(amount)
		case core.EntryTypeCredit:
			current.Credit = current.Credit.Add(amount)
		}
		totalsByCurrency[row.CurrencyID] = current
	}

	result := make([]service.CurrencyReconcileTotals, 0, len(totalsByCurrency))
	for _, totals := range totalsByCurrency {
		result = append(result, totals)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CurrencyID < result[j].CurrencyID })
	return result, nil
}

// --- AccountEntrySummer ---

func (a *RollupAdapter) SumEntriesByAccountClassification(ctx context.Context, holder, currencyID int64) (debitByClass, creditByClass map[int64]decimal.Decimal, err error) {
	rows, err := a.q.SumEntriesByAccountClassification(ctx, sqlcgen.SumEntriesByAccountClassificationParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("postgres: sum by account: %w", err)
	}

	debitByClass = make(map[int64]decimal.Decimal)
	creditByClass = make(map[int64]decimal.Decimal)
	for _, r := range rows {
		amount, err := anyToDecimal(r.Total)
		if err != nil {
			return nil, nil, fmt.Errorf("postgres: sum by account: convert: %w", err)
		}
		switch core.EntryType(r.EntryType) {
		case core.EntryTypeDebit:
			debitByClass[r.ClassificationID] = amount
		case core.EntryTypeCredit:
			creditByClass[r.ClassificationID] = amount
		}
	}
	return debitByClass, creditByClass, nil
}

// --- CheckpointReader ---

func (a *RollupAdapter) GetCheckpoints(ctx context.Context, holder, currencyID int64) ([]service.BalanceCheckpoint, error) {
	rows, err := a.q.GetBalanceCheckpoints(ctx, sqlcgen.GetBalanceCheckpointsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get checkpoints: %w", err)
	}
	result := make([]service.BalanceCheckpoint, len(rows))
	for i, r := range rows {
		result[i] = service.BalanceCheckpoint{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			Balance:          mustNumericToDecimal(r.Balance),
			LastEntryID:      r.LastEntryID,
			LastEntryAt:      r.LastEntryAt,
			UpdatedAt:        r.UpdatedAt,
		}
	}
	return result, nil
}

// --- CheckpointLister ---

func (a *RollupAdapter) ListAllCheckpoints(ctx context.Context) ([]service.BalanceCheckpoint, error) {
	rows, err := a.q.ListAllBalanceCheckpoints(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list all checkpoints: %w", err)
	}
	result := make([]service.BalanceCheckpoint, len(rows))
	for i, r := range rows {
		result[i] = service.BalanceCheckpoint{
			AccountHolder:    r.AccountHolder,
			CurrencyID:       r.CurrencyID,
			ClassificationID: r.ClassificationID,
			Balance:          mustNumericToDecimal(r.Balance),
			LastEntryID:      r.LastEntryID,
			LastEntryAt:      r.LastEntryAt,
			UpdatedAt:        r.UpdatedAt,
		}
	}
	return result, nil
}

// --- SnapshotWriter ---

func (a *RollupAdapter) ListBalancesAt(ctx context.Context, cutoff time.Time) ([]core.Balance, error) {
	rows, err := a.q.ListBalancesAt(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("postgres: list balances at %s: %w", cutoff.Format(time.RFC3339), err)
	}

	balances := make([]core.Balance, 0, len(rows))
	for _, row := range rows {
		cur, err := a.dims.currencyByIDOrErr(ctx, a.q, row.CurrencyID)
		if err != nil {
			return nil, err
		}
		cls, err := a.dims.classByIDOrErr(ctx, a.q, row.ClassificationID)
		if err != nil {
			return nil, err
		}
		balances = append(balances, core.Balance{
			AccountHolder:     row.AccountHolder,
			CurrencyUID:       cur.UID,
			ClassificationUID: cls.UID,
			Balance:           mustNumericToDecimal(row.Balance),
		})
	}

	return balances, nil
}

func (a *RollupAdapter) UpsertSnapshot(ctx context.Context, snap core.BalanceSnapshot) error {
	cur, err := a.dims.currencyByUIDOrErr(ctx, a.q, snap.CurrencyUID)
	if err != nil {
		return err
	}
	cls, err := a.dims.classByUIDOrErr(ctx, a.q, snap.ClassificationUID)
	if err != nil {
		return err
	}
	return a.q.InsertSnapshot(ctx, sqlcgen.InsertSnapshotParams{
		AccountHolder:    snap.AccountHolder,
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
		SnapshotDate:     pgtype.Date{Time: snap.SnapshotDate, Valid: true},
		Balance:          decimalToNumeric(snap.Balance),
	})
}

// GetSnapshotBalances reads the stored balance_snapshots row for (holder,
// currency, date). A snapshot is computed once, from whatever entries
// existed at that moment (service/snapshot.go's CreateDailySnapshot); nothing
// re-triggers it when a later write retroactively backdates (effective_at
// earlier than the snapshot's own creation) into an already-snapshotted
// business date. So before trusting a cached row, it is checked against
// journal_entries for exactly that condition and recomputed live -- bypassing
// the stale cache -- when found stale. See
// docs/audits/2026-08-25-financial-engineering/financial-correctness.md
// Major #2 ("effective_at 回溯记账不会让已写入的历史快照失效").
func (a *RollupAdapter) GetSnapshotBalances(ctx context.Context, holder int64, currencyUID string, date time.Time) ([]core.Balance, error) {
	cur, err := a.dims.currencyByUIDOrErr(ctx, a.q, currencyUID)
	if err != nil {
		return nil, err
	}
	rows, err := a.q.GetSnapshotBalances(ctx, sqlcgen.GetSnapshotBalancesParams{
		AccountHolder: holder,
		CurrencyID:    cur.ID,
		SnapshotDate:  pgtype.Date{Time: date, Valid: true},
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get snapshot balances: %w", err)
	}

	// Exclusive upper bound matching CreateDailySnapshot's own cutoff
	// (service/snapshot.go: cutoff = snapshotDate.AddDate(0, 0, 1)):
	// "as of the end of `date`" == effective_at < date+1.
	cutoff := date.AddDate(0, 0, 1)

	if len(rows) == 0 {
		return []core.Balance{}, nil
	}

	// Staleness is decided for the whole (holder, currency) at once, against
	// the OLDEST snapshot row of the date, rather than row by row.
	//
	// The row-by-row version could only ever ask "was this cached row
	// overtaken", which is unanswerable for a dimension that has no cached
	// row at all -- and backdating a journal into an already-snapshotted date
	// can introduce a classification that did not exist when the snapshot
	// ran. Measured: a backdated write that raised main_wallet from 100 to
	// 150 and opened a pending position of 70 produced the corrected 150 and
	// no pending row whatsoever. A missing dimension is worse than a wrong
	// number: an as-of position short by 70 looks exactly like a position
	// that was never taken. Sparse snapshots (snapshot_extra_store.go writes
	// no row when a balance did not move) make "no row" routine, so this is
	// the common shape rather than an edge (audit A-M5).
	oldestSnapshotAt := rows[0].CreatedAt
	for _, r := range rows[1:] {
		if r.CreatedAt.Before(oldestSnapshotAt) {
			oldestSnapshotAt = r.CreatedAt
		}
	}
	stale, err := a.snapshotIsStale(ctx, holder, cur.ID, cutoff, oldestSnapshotAt)
	if err != nil {
		return nil, fmt.Errorf("postgres: get snapshot balances: staleness check: %w", err)
	}

	if !stale {
		result := make([]core.Balance, len(rows))
		for i, r := range rows {
			cls, err := a.dims.classByIDOrErr(ctx, a.q, r.ClassificationID)
			if err != nil {
				return nil, err
			}
			result[i] = core.Balance{
				AccountHolder:     r.AccountHolder,
				CurrencyUID:       currencyUID,
				ClassificationUID: cls.UID,
				Balance:           mustNumericToDecimal(r.Balance),
			}
		}
		return result, nil
	}

	// Full outer join of the cached dimension set against the live one:
	// cached-but-no-longer-live reads zero, live-but-not-cached is added.
	live, err := a.liveBalancesByClassificationAt(ctx, holder, cur.ID, cutoff)
	if err != nil {
		return nil, err
	}

	classificationIDs := make([]int64, 0, len(rows)+len(live))
	seen := make(map[int64]struct{}, len(rows)+len(live))
	for _, r := range rows {
		if _, ok := seen[r.ClassificationID]; ok {
			continue
		}
		seen[r.ClassificationID] = struct{}{}
		classificationIDs = append(classificationIDs, r.ClassificationID)
	}
	extra := make([]int64, 0, len(live))
	for id := range live {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		extra = append(extra, id)
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i] < extra[j] })
	classificationIDs = append(classificationIDs, extra...)

	result := make([]core.Balance, 0, len(classificationIDs))
	for _, id := range classificationIDs {
		cls, err := a.dims.classByIDOrErr(ctx, a.q, id)
		if err != nil {
			return nil, err
		}
		result = append(result, core.Balance{
			AccountHolder:     holder,
			CurrencyUID:       currencyUID,
			ClassificationUID: cls.UID,
			Balance:           live[id], // zero value when the dimension no longer has entries as of cutoff
		})
	}
	return result, nil
}

// snapshotIsStale reports whether ANY journal_entries write for this
// (holder, currency), backdated (effective_at < cutoff) into the snapshotted
// window, landed after the date's snapshot was written -- i.e. after a value
// that could not have included it. Deliberately dimension-agnostic; see
// GetSnapshotBalances for why a per-dimension test cannot see a dimension
// that the backdated write itself created.
func (a *RollupAdapter) snapshotIsStale(ctx context.Context, holder, currencyID int64, cutoff, snapshotCreatedAt time.Time) (bool, error) {
	raw, err := a.q.GetMaxEntryCreatedAtForHolderCurrencyBefore(ctx, sqlcgen.GetMaxEntryCreatedAtForHolderCurrencyBeforeParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		EffectiveAt:   cutoff,
	})
	if err != nil {
		return false, err
	}
	maxCreatedAt, err := anyToTime(raw)
	if err != nil {
		return false, err
	}
	return maxCreatedAt.After(snapshotCreatedAt), nil
}

// liveBalancesByClassificationAt recomputes every classification's balance
// for one (holder, currency) as of cutoff directly from journal_entries,
// bypassing the balance_snapshots cache entirely. Reuses ListBalancesAt's
// query (the normal_side/entry_type sign convention already implemented
// there) instead of re-implementing it a third time in this file -- the
// audit that found this bug separately flagged 17 independent copies of that
// exact convention as its own Minor; this fix must not add an 18th.
func (a *RollupAdapter) liveBalancesByClassificationAt(ctx context.Context, holder, currencyID int64, cutoff time.Time) (map[int64]decimal.Decimal, error) {
	rows, err := a.q.ListBalancesAtForHolderCurrency(ctx, sqlcgen.ListBalancesAtForHolderCurrencyParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
		EffectiveAt:   cutoff,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: get snapshot balances: live recompute: %w", err)
	}
	out := make(map[int64]decimal.Decimal, len(rows))
	for _, row := range rows {
		out[row.ClassificationID] = mustNumericToDecimal(row.Balance)
	}
	return out, nil
}

// --- CheckpointAggregator ---

func (a *RollupAdapter) AggregateCheckpointsByClassification(ctx context.Context) ([]core.SystemRollup, error) {
	rows, err := a.q.AggregateCheckpointsByClassification(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: aggregate checkpoints: %w", err)
	}
	result := make([]core.SystemRollup, len(rows))
	for i, r := range rows {
		bal, err := anyToDecimal(r.TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("postgres: aggregate checkpoints: convert: %w", err)
		}
		cur, err := a.dims.currencyByIDOrErr(ctx, a.q, r.CurrencyID)
		if err != nil {
			return nil, err
		}
		cls, err := a.dims.classByIDOrErr(ctx, a.q, r.ClassificationID)
		if err != nil {
			return nil, err
		}
		result[i] = core.SystemRollup{
			CurrencyUID:       cur.UID,
			ClassificationUID: cls.UID,
			TotalBalance:      bal,
		}
	}
	return result, nil
}

// --- SystemRollupWriter ---

func (a *RollupAdapter) UpsertSystemRollup(ctx context.Context, rollup core.SystemRollup) error {
	cur, err := a.dims.currencyByUIDOrErr(ctx, a.q, rollup.CurrencyUID)
	if err != nil {
		return err
	}
	cls, err := a.dims.classByUIDOrErr(ctx, a.q, rollup.ClassificationUID)
	if err != nil {
		return err
	}
	return a.q.UpsertSystemRollup(ctx, sqlcgen.UpsertSystemRollupParams{
		CurrencyID:       cur.ID,
		ClassificationID: cls.ID,
		TotalBalance:     decimalToNumeric(rollup.TotalBalance),
	})
}

// --- ExpiredReservationFinder ---

func (a *RollupAdapter) GetExpiredReservations(ctx context.Context, limit int) ([]core.Reservation, error) {
	rows, err := a.q.GetExpiredReservations(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: get expired reservations: %w", err)
	}
	result := make([]core.Reservation, len(rows))
	for i, r := range rows {
		res, err := reservationFromRow(ctx, a.dims, a.q, r)
		if err != nil {
			return nil, err
		}
		result[i] = *res
	}
	return result, nil
}

// --- service.ClassificationLister (dimension port) ---

// ClassificationDims exposes the classification dimension rows (internal id +
// uid + normal side) the rollup/reconcile math is keyed on.
func (a *RollupAdapter) ClassificationDims(ctx context.Context) ([]service.ClassificationDim, error) {
	rows, err := a.q.ListClassificationDims(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: classification dims: %w", err)
	}
	out := make([]service.ClassificationDim, len(rows))
	for i, r := range rows {
		out[i] = service.ClassificationDim{
			ID:         r.ID,
			UID:        pgToUID(r.Uid),
			Code:       r.Code,
			NormalSide: core.NormalSide(r.NormalSide),
		}
	}
	return out, nil
}

// CurrencyIDByUID resolves a currency uid to its internal id.
func (a *RollupAdapter) CurrencyIDByUID(ctx context.Context, uid string) (int64, error) {
	d, err := a.dims.currencyByUIDOrErr(ctx, a.q, uid)
	if err != nil {
		return 0, err
	}
	return d.ID, nil
}

// CurrencyUIDByID resolves an internal currency id to its uid.
func (a *RollupAdapter) CurrencyUIDByID(ctx context.Context, id int64) (string, error) {
	d, err := a.dims.currencyByIDOrErr(ctx, a.q, id)
	if err != nil {
		return "", err
	}
	return d.UID, nil
}
