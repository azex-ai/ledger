package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PartitionStore manages the monthly range partitions of journal_entries
// (see migration 037 and docs/INVARIANTS.md I-13). DDL cannot be
// parameterized; every interpolated value below is derived from time.Time —
// no user input reaches the SQL.
//
// Every operation goes through the SECURITY DEFINER functions migration 007
// installs (ledger_create_monthly_partition / ledger_rebalance_default_partition)
// rather than issuing DDL directly. Partition creation, DETACH/ATTACH and
// TRUNCATE are all owner-gated in Postgres, and the pool passed in here is
// the ordinary ledger_app serving pool — it holds none of those grants and
// was never meant to. The two functions run with their owner's (ledger_owner)
// privileges regardless of caller, so PartitionStore needs nothing beyond
// EXECUTE. See migration 007's header comment for why: giving the serving
// pool ledger_owner instead (the only alternative that made the old
// direct-DDL version work) also hands it a bare TRUNCATE that walks straight
// past journal_entries' no-DELETE trigger, which does not fire on TRUNCATE.
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

// createPartition calls ledger_create_monthly_partition for one month.
// Returns whether the table was newly created.
func (s *PartitionStore) createPartition(ctx context.Context, month time.Time) (bool, error) {
	name := partitionName(month)
	next := month.AddDate(0, 1, 0)
	var didCreate bool
	if err := s.pool.QueryRow(ctx,
		"SELECT ledger_create_monthly_partition($1, $2, $3)",
		name, month.Format("2006-01-02"), next.Format("2006-01-02"),
	).Scan(&didCreate); err != nil {
		return false, fmt.Errorf("postgres: partition: create %s: %w", name, err)
	}
	return didCreate, nil
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

// rebalanceDefault calls ledger_rebalance_default_partition, which performs
// the migration-037 dance at runtime: detach the default partition, create
// every monthly partition needed to cover its rows plus the requested
// horizon, move the rows into their monthly homes, and re-attach the emptied
// default — all inside the function's own implicit transaction, so this is
// a single round trip rather than a hand-managed pgx transaction.
//
// LOCKING TRADEOFF: DETACH/ATTACH inside the function are non-CONCURRENT
// (CONCURRENTLY is forbidden inside a transaction, and this dance needs
// atomicity), so the whole statement — including the bulk row move — holds
// an ACCESS EXCLUSIVE lock on journal_entries, blocking every ledger read
// and write until it returns. With an active partition job the default
// partition is empty or near-empty and this is milliseconds; it only
// becomes expensive after the horizon has already lapsed. See RUNBOOK §11.
func (s *PartitionStore) rebalanceDefault(ctx context.Context, now time.Time, monthsAhead int) ([]string, error) {
	first := monthStart(now.UTC())
	last := first.AddDate(0, monthsAhead, 0)

	var created []string
	rows, err := s.pool.Query(ctx,
		"SELECT unnest(ledger_rebalance_default_partition($1, $2))",
		first.Format("2006-01-02"), last.Format("2006-01-02"),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: partition: rebalance: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("postgres: partition: rebalance: scan: %w", err)
		}
		created = append(created, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: partition: rebalance: %w", err)
	}
	return created, nil
}
