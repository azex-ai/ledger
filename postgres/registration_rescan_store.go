package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
)

var _ core.RegistrationRescanStore = (*RegistrationRescanStore)(nil)

// RegistrationRescanStore is the PostgreSQL-backed durable work queue for
// address-registration history scans.
type RegistrationRescanStore struct{ pool *pgxpool.Pool }

func NewRegistrationRescanStore(pool *pgxpool.Pool) *RegistrationRescanStore {
	return &RegistrationRescanStore{pool: pool}
}

func (s *RegistrationRescanStore) EnqueueRegistrationRescans(ctx context.Context, jobs []core.RegistrationRescan) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: enqueue registration rescans: %w", err)
	}
	defer tx.Rollback(ctx)
	for _, job := range jobs {
		if job.ChainID <= 0 || job.Address == "" || job.NextBlock < 0 {
			return fmt.Errorf("postgres: enqueue registration rescans: invalid job: %w", core.ErrInvalidInput)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO registration_rescans (uid, chain_id, address, next_block)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (chain_id, address) DO NOTHING`, newUID(), job.ChainID, job.Address, job.NextBlock)
		if err != nil {
			return fmt.Errorf("postgres: enqueue registration rescans: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: enqueue registration rescans: commit: %w", err)
	}
	return nil
}

func (s *RegistrationRescanStore) ClaimRegistrationRescans(ctx context.Context, limit int, lease time.Duration) ([]core.RegistrationRescan, error) {
	if limit <= 0 || lease <= 0 {
		return nil, fmt.Errorf("postgres: claim registration rescans: invalid limit or lease: %w", core.ErrInvalidInput)
	}
	rows, err := s.pool.Query(ctx, `
		WITH candidates AS (
			SELECT id FROM registration_rescans
			WHERE status <> 'completed'
			  AND available_at <= now()
			  AND (status = 'pending' OR claimed_until < now())
			ORDER BY available_at, id
			FOR UPDATE SKIP LOCKED
			LIMIT $1
		)
		UPDATE registration_rescans r
		SET status = 'running', claimed_until = now() + $2::interval,
		    attempts = attempts + 1, updated_at = now()
		FROM candidates c WHERE r.id = c.id
		RETURNING r.uid::text, r.chain_id, r.address, r.next_block, r.attempts`, limit, lease.String())
	if err != nil {
		return nil, fmt.Errorf("postgres: claim registration rescans: %w", err)
	}
	defer rows.Close()
	var out []core.RegistrationRescan
	for rows.Next() {
		var job core.RegistrationRescan
		if err := rows.Scan(&job.UID, &job.ChainID, &job.Address, &job.NextBlock, &job.Attempts); err != nil {
			return nil, fmt.Errorf("postgres: claim registration rescans: scan: %w", err)
		}
		out = append(out, job)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: claim registration rescans: rows: %w", err)
	}
	return out, nil
}

func (s *RegistrationRescanStore) AdvanceRegistrationRescan(ctx context.Context, uid string, nextBlock int64, completed bool) error {
	status := "pending"
	if completed {
		status = "completed"
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE registration_rescans
		SET next_block = $2, status = $3, available_at = now(),
		    claimed_until = NULL, last_error = NULL, updated_at = now()
		WHERE uid = $1::uuid`, uid, nextBlock, status)
	if err != nil {
		return fmt.Errorf("postgres: advance registration rescan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: advance registration rescan: %w", pgx.ErrNoRows)
	}
	return nil
}

func (s *RegistrationRescanStore) RetryRegistrationRescan(ctx context.Context, uid, lastError string, retryAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE registration_rescans
		SET status = 'pending', available_at = $2, claimed_until = NULL,
		    last_error = left($3, 2000), updated_at = now()
		WHERE uid = $1::uuid`, uid, retryAt, lastError)
	if err != nil {
		return fmt.Errorf("postgres: retry registration rescan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: retry registration rescan: %w", pgx.ErrNoRows)
	}
	return nil
}
