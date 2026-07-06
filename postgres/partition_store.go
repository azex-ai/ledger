package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionStore manages the monthly range partitions of journal_entries
// (see migration 037 and docs/INVARIANTS.md I-13). DDL cannot be
// parameterized; every interpolated value below is derived from time.Time —
// no user input reaches the SQL.
type PartitionStore struct {
	pool *pgxpool.Pool
}

// NewPartitionStore creates a PartitionStore.
func NewPartitionStore(pool *pgxpool.Pool) *PartitionStore {
	return &PartitionStore{pool: pool}
}

func partitionName(month time.Time) string {
	return fmt.Sprintf("journal_entries_y%04dm%02d", month.Year(), int(month.Month()))
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// EnsureMonthlyPartitions creates (idempotently) the monthly partitions
// covering [current month .. current month + monthsAhead]. When creation
// fails because the default partition holds rows inside a target range
// (possible only if the horizon was allowed to lapse), it falls back to a
// rebalance: detach default → create partitions → move rows → re-attach
// empty default, all in one transaction. Returns the names of partitions
// actually created.
func (s *PartitionStore) EnsureMonthlyPartitions(ctx context.Context, now time.Time, monthsAhead int) ([]string, error) {
	if monthsAhead < 1 {
		monthsAhead = 1
	}
	start := monthStart(now.UTC())

	var created []string
	for i := 0; i <= monthsAhead; i++ {
		month := start.AddDate(0, i, 0)
		didCreate, err := s.createPartition(ctx, month)
		if err != nil {
			// Only escalate to the (heavily locking) rebalance when the
			// failure is specifically "default partition holds rows in this
			// range" (SQLSTATE 23514). Transient errors (timeouts, network)
			// must surface to the worker's error log instead of triggering
			// a full-table lock.
			if !isDefaultOverlapError(err) {
				return created, err
			}
			rebalanced, rbErr := s.rebalanceDefault(ctx, now, monthsAhead)
			if rbErr != nil {
				return created, fmt.Errorf("postgres: partition: create %s failed (%w); rebalance also failed: %w", partitionName(month), err, rbErr)
			}
			return append(created, rebalanced...), nil
		}
		if didCreate {
			created = append(created, partitionName(month))
		}
	}
	return created, nil
}

// createPartition issues CREATE TABLE IF NOT EXISTS for one month. Returns
// whether the table was newly created.
func (s *PartitionStore) createPartition(ctx context.Context, month time.Time) (bool, error) {
	name := partitionName(month)
	var exists bool
	if err := s.pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", name).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres: partition: check %s: %w", name, err)
	}
	if exists {
		return false, nil
	}
	next := month.AddDate(0, 1, 0)
	sql := fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF journal_entries FOR VALUES FROM ('%s') TO ('%s')",
		name, month.Format("2006-01-02"), next.Format("2006-01-02"),
	)
	if _, err := s.pool.Exec(ctx, sql); err != nil {
		return false, fmt.Errorf("postgres: partition: create %s: %w", name, err)
	}
	return true, nil
}

// DefaultPartitionHasRows reports whether journal_entries_default holds any
// rows — with an active partition job this should always be false; true is
// an alertable signal (rows are landing outside every named partition).
func (s *PartitionStore) DefaultPartitionHasRows(ctx context.Context) (bool, error) {
	var hasRows bool
	err := s.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM journal_entries_default)").Scan(&hasRows)
	if err != nil {
		return false, fmt.Errorf("postgres: partition: default rows check: %w", err)
	}
	return hasRows, nil
}

// RebalanceDefault exposes the default-partition rebalance for the worker's
// partition job: when rows are found stranded in the default partition, the
// job calls this directly (the fast-path CREATE in EnsureMonthlyPartitions
// only trips over rows inside its forward horizon — stranded rows in past
// months need this explicit path).
func (s *PartitionStore) RebalanceDefault(ctx context.Context, now time.Time, monthsAhead int) ([]string, error) {
	return s.rebalanceDefault(ctx, now, monthsAhead)
}

// isDefaultOverlapError reports whether err is PostgreSQL check_violation
// (SQLSTATE 23514) — raised when creating a partition whose range overlaps
// rows currently in the default partition. That is the only error the
// rebalance fallback should fire on.
func isDefaultOverlapError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

// rebalanceDefault performs the migration-037 dance at runtime: inside one
// transaction, detach the default partition, create every monthly partition
// needed to cover its rows plus the requested horizon, move the rows into
// their monthly homes, and re-attach the emptied default.
//
// LOCKING TRADEOFF: DETACH/ATTACH here are non-CONCURRENT (CONCURRENTLY is
// forbidden inside a transaction, and this dance needs atomicity), so the
// whole transaction — including the bulk row move — holds an ACCESS
// EXCLUSIVE lock on journal_entries, blocking every ledger read and write
// until it commits. With an active partition job the default partition is
// empty or near-empty and this is milliseconds; it only becomes expensive
// after the horizon has already lapsed. See RUNBOOK §11.
func (s *PartitionStore) rebalanceDefault(ctx context.Context, now time.Time, monthsAhead int) ([]string, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("postgres: partition: begin rebalance: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "ALTER TABLE journal_entries DETACH PARTITION journal_entries_default"); err != nil {
		return nil, fmt.Errorf("postgres: partition: detach default: %w", err)
	}

	var minAt, maxAt *time.Time
	if err := tx.QueryRow(ctx,
		"SELECT min(created_at), max(created_at) FROM journal_entries_default",
	).Scan(&minAt, &maxAt); err != nil {
		return nil, fmt.Errorf("postgres: partition: scan default range: %w", err)
	}

	first := monthStart(now.UTC())
	last := first.AddDate(0, monthsAhead, 0)
	if minAt != nil {
		if m := monthStart(minAt.UTC()); m.Before(first) {
			first = m
		}
		if m := monthStart(maxAt.UTC()); m.After(last) {
			last = m
		}
	}

	var created []string
	for month := first; !month.After(last); month = month.AddDate(0, 1, 0) {
		name := partitionName(month)
		sql := fmt.Sprintf(
			"CREATE TABLE IF NOT EXISTS %s PARTITION OF journal_entries FOR VALUES FROM ('%s') TO ('%s')",
			name, month.Format("2006-01-02"), month.AddDate(0, 1, 0).Format("2006-01-02"),
		)
		if _, err := tx.Exec(ctx, sql); err != nil {
			return nil, fmt.Errorf("postgres: partition: rebalance create %s: %w", name, err)
		}
		created = append(created, name)
	}

	if minAt != nil {
		if _, err := tx.Exec(ctx, "INSERT INTO journal_entries SELECT * FROM journal_entries_default"); err != nil {
			return nil, fmt.Errorf("postgres: partition: move default rows: %w", err)
		}
		if _, err := tx.Exec(ctx, "TRUNCATE journal_entries_default"); err != nil {
			return nil, fmt.Errorf("postgres: partition: truncate default: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "ALTER TABLE journal_entries ATTACH PARTITION journal_entries_default DEFAULT"); err != nil {
		return nil, fmt.Errorf("postgres: partition: re-attach default: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: partition: commit rebalance: %w", err)
	}
	return created, nil
}
