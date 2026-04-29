package service

import (
	"context"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
)

// HistoricalBalanceLister lists balances as of an exclusive upper-bound timestamp.
type HistoricalBalanceLister interface {
	ListBalancesAt(ctx context.Context, cutoff time.Time) ([]core.Balance, error)
}

// SnapshotWriter writes and reads balance snapshots.
type SnapshotWriter interface {
	UpsertSnapshot(ctx context.Context, snap core.BalanceSnapshot) error
	GetSnapshotBalances(ctx context.Context, holder, currencyID int64, date time.Time) ([]core.Balance, error)
}

// snapshotLockAcquirer wraps the advisory-lock helpers so they can be
// overridden in tests without a real Postgres pool.
type snapshotLockAcquirer interface {
	tryAdvisoryLock(ctx context.Context, key int64) (bool, error)
	releaseAdvisoryLock(ctx context.Context, key int64) error
}

// pgAdvisoryLock implements snapshotLockAcquirer using pg_try_advisory_lock.
type pgAdvisoryLock struct{ pool *pgxpool.Pool }

func (p *pgAdvisoryLock) tryAdvisoryLock(ctx context.Context, key int64) (bool, error) {
	var acquired bool
	err := p.pool.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired)
	if err != nil {
		return false, fmt.Errorf("service: snapshot: pg_try_advisory_lock: %w", err)
	}
	return acquired, nil
}

func (p *pgAdvisoryLock) releaseAdvisoryLock(ctx context.Context, key int64) error {
	var released bool
	if err := p.pool.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", key).Scan(&released); err != nil {
		return fmt.Errorf("service: snapshot: pg_advisory_unlock: %w", err)
	}
	if !released {
		return fmt.Errorf("service: snapshot: pg_advisory_unlock returned false for key %d", key)
	}
	return nil
}

// advisoryLockKey computes a stable int64 key from a name string using FNV-64a.
func advisoryLockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	// Cast to int64 — wrapping on overflow is intentional; PG accepts the full int64 range.
	return int64(h.Sum64())
}

// SnapshotService handles daily balance snapshots.
type SnapshotService struct {
	balances  HistoricalBalanceLister
	snapshots SnapshotWriter
	sparse    core.SparseSnapshotter // nil → non-sparse fallback
	locker    snapshotLockAcquirer   // nil → skip advisory lock (no pool available)
	logger    core.Logger
	metrics   core.Metrics
}

// NewSnapshotService creates a new SnapshotService.
func NewSnapshotService(
	balances HistoricalBalanceLister,
	snapshots SnapshotWriter,
	engine *core.Engine,
) *SnapshotService {
	return &SnapshotService{
		balances:  balances,
		snapshots: snapshots,
		logger:    engine.Logger(),
		metrics:   engine.Metrics(),
	}
}

// WithPool attaches a *pgxpool.Pool to enable pg_try_advisory_lock-based
// multi-replica safety. Call this before using CreateDailySnapshot in a
// long-running service; library users that run snapshots inline don't need it.
func (s *SnapshotService) WithPool(pool *pgxpool.Pool) *SnapshotService {
	if pool != nil {
		s.locker = &pgAdvisoryLock{pool: pool}
	}
	return s
}

// WithSparseSnapshotter attaches a SparseSnapshotter so that CreateDailySnapshot
// only writes rows whose balance has changed since the previous snapshot.
func (s *SnapshotService) WithSparseSnapshotter(ss core.SparseSnapshotter) *SnapshotService {
	s.sparse = ss
	return s
}

// CreateDailySnapshot recomputes balances as of the end of the given day and
// stores them as snapshots.  Multi-replica safety: when a pool was registered
// via WithPool, the method acquires pg_try_advisory_lock before writing; if
// the lock is already held by another pod the call returns nil immediately
// (skip-and-log semantics).  Idempotency: existing rows are upserted so
// re-running for the same date is safe.  Sparse storage: when a
// SparseSnapshotter is registered, rows are only written when the balance
// differs from the most recent existing snapshot.
func (s *SnapshotService) CreateDailySnapshot(ctx context.Context, date time.Time) error {
	snapshotDate := normalizeDay(date)
	cutoff := snapshotDate.AddDate(0, 0, 1)

	// Advisory lock key: "snapshot:" + YYYYMMDD
	lockName := "snapshot:" + snapshotDate.Format("20060102")
	lockKey := advisoryLockKey(lockName)

	if s.locker != nil {
		acquired, err := s.locker.tryAdvisoryLock(ctx, lockKey)
		if err != nil {
			s.logger.Error("service: snapshot: advisory lock failed, proceeding without lock",
				"date", snapshotDate.Format("2006-01-02"),
				"error", err,
			)
		} else if !acquired {
			s.logger.Info("service: snapshot: advisory lock held by another replica, skipping",
				"date", snapshotDate.Format("2006-01-02"),
			)
			return nil
		} else {
			defer func() {
				if err := s.locker.releaseAdvisoryLock(ctx, lockKey); err != nil {
					s.logger.Error("service: snapshot: release advisory lock failed",
						"date", snapshotDate.Format("2006-01-02"),
						"error", err,
					)
				}
			}()
		}
	}

	start := time.Now()

	balances, err := s.balances.ListBalancesAt(ctx, cutoff)
	if err != nil {
		return fmt.Errorf("service: snapshot: list balances at %s: %w", cutoff.Format(time.RFC3339), err)
	}

	written := 0
	for _, balance := range balances {
		snap := core.BalanceSnapshot{
			AccountHolder:    balance.AccountHolder,
			CurrencyID:       balance.CurrencyID,
			ClassificationID: balance.ClassificationID,
			SnapshotDate:     snapshotDate,
			Balance:          balance.Balance,
		}

		if s.sparse != nil {
			ok, err := s.sparse.UpsertSnapshotSparse(ctx, snap)
			if err != nil {
				return fmt.Errorf("service: snapshot: sparse insert: holder=%d currency=%d class=%d: %w",
					balance.AccountHolder, balance.CurrencyID, balance.ClassificationID, err)
			}
			if ok {
				written++
			}
		} else {
			if err := s.snapshots.UpsertSnapshot(ctx, snap); err != nil {
				return fmt.Errorf("service: snapshot: insert: holder=%d currency=%d class=%d: %w",
					balance.AccountHolder, balance.CurrencyID, balance.ClassificationID, err)
			}
			written++
		}
	}

	s.metrics.SnapshotLatency(time.Since(start))
	s.logger.Info("service: snapshot: daily snapshot created",
		"date", snapshotDate.Format("2006-01-02"),
		"accounts_checked", len(balances),
		"snapshots_written", written,
	)

	return nil
}

// GetSnapshotBalance reads balance snapshots for a specific holder, currency, and date.
func (s *SnapshotService) GetSnapshotBalance(ctx context.Context, holder int64, currencyID int64, date time.Time) ([]core.Balance, error) {
	snapshotDate := normalizeDay(date)

	balances, err := s.snapshots.GetSnapshotBalances(ctx, holder, currencyID, snapshotDate)
	if err != nil {
		return nil, fmt.Errorf("service: snapshot: get balances: %w", err)
	}

	return balances, nil
}

// Compile-time interface check.
var _ core.Snapshotter = (*SnapshotService)(nil)
