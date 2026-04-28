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

var _ core.CurrencyStore = (*CurrencyStore)(nil)

// CurrencyStore implements core.CurrencyStore using PostgreSQL.
//
// In pool mode (constructed via NewCurrencyStore), queries run against the
// pool. In tx mode (bound via withDB), queries participate in the caller's
// transaction.
type CurrencyStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewCurrencyStore creates a new CurrencyStore.
func NewCurrencyStore(pool *pgxpool.Pool) *CurrencyStore {
	return &CurrencyStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// WithDB returns a clone of the CurrencyStore bound to an existing transaction.
func (s *CurrencyStore) WithDB(db DBTX) *CurrencyStore {
	return &CurrencyStore{
		pool: nil, // tx mode
		q:    sqlcgen.New(db),
	}
}

// CreateCurrency inserts a new currency.
func (s *CurrencyStore) CreateCurrency(ctx context.Context, input core.CurrencyInput) (*core.Currency, error) {
	row, err := s.q.CreateCurrency(ctx, sqlcgen.CreateCurrencyParams{
		Code: input.Code,
		Name: input.Name,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: create currency", err)
	}
	return currencyFromRow(row), nil
}

// ListCurrencies returns all currencies.
func (s *CurrencyStore) ListCurrencies(ctx context.Context) ([]core.Currency, error) {
	rows, err := s.q.ListCurrencies(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list currencies: %w", err)
	}
	result := make([]core.Currency, len(rows))
	for i, row := range rows {
		result[i] = *currencyFromRow(row)
	}
	return result, nil
}

// GetCurrency retrieves a currency by ID.
func (s *CurrencyStore) GetCurrency(ctx context.Context, id int64) (*core.Currency, error) {
	row, err := s.q.GetCurrency(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get currency: id %d: %w", id, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get currency: %w", err)
	}
	return currencyFromRow(row), nil
}
