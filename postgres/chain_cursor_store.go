package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.ChainCursorStore = (*ChainCursorStore)(nil)

// ChainCursorStore implements core.ChainCursorStore using PostgreSQL.
type ChainCursorStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewChainCursorStore creates a ChainCursorStore backed by a connection pool.
func NewChainCursorStore(pool *pgxpool.Pool) *ChainCursorStore {
	return &ChainCursorStore{pool: pool, db: pool, q: sqlcgen.New(pool)}
}

// WithDB returns a clone of the ChainCursorStore bound to an existing transaction.
func (s *ChainCursorStore) WithDB(db DBTX) *ChainCursorStore {
	return &ChainCursorStore{pool: nil, db: db, q: sqlcgen.New(db)}
}

// GetCursor returns chainID's cursor, or core.ErrNotFound if it has never
// been scanned.
func (s *ChainCursorStore) GetCursor(ctx context.Context, chainID int64) (*core.ChainCursor, error) {
	row, err := s.q.GetChainCursor(ctx, chainID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get chain cursor %d: %w", chainID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get chain cursor %d: %w", chainID, err)
	}
	return &core.ChainCursor{
		ChainID:          row.ChainID,
		LastScannedBlock: row.LastScannedBlock,
		UpdatedAt:        row.UpdatedAt,
	}, nil
}

// SetCursor advances chainID's cursor (upsert).
func (s *ChainCursorStore) SetCursor(ctx context.Context, chainID int64, lastScannedBlock int64) error {
	if err := s.q.SetChainCursor(ctx, sqlcgen.SetChainCursorParams{
		ChainID:          chainID,
		LastScannedBlock: lastScannedBlock,
	}); err != nil {
		return fmt.Errorf("postgres: set chain cursor %d: %w", chainID, err)
	}
	return nil
}
