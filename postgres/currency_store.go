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
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: create currency: %w", err)
	}
	row, err := s.q.CreateCurrency(ctx, sqlcgen.CreateCurrencyParams{
		Code:     input.Code,
		Name:     input.Name,
		Exponent: int16(input.Exponent),
		Uid:      newUID(),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: create currency", err)
	}
	return currencyFromRow(row), nil
}

// DeactivateCurrency soft-deletes a currency by setting is_active = false.
func (s *CurrencyStore) DeactivateCurrency(ctx context.Context, uid string) error {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return err
	}
	if err := s.q.DeactivateCurrency(ctx, pgUID); err != nil {
		return wrapStoreError("postgres: deactivate currency", err)
	}
	return nil
}

// ListCurrencies returns currencies, optionally filtering to active only.
func (s *CurrencyStore) ListCurrencies(ctx context.Context, activeOnly bool) ([]core.Currency, error) {
	rows, err := s.q.ListCurrencies(ctx, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("postgres: list currencies: %w", err)
	}
	result := make([]core.Currency, len(rows))
	for i, row := range rows {
		result[i] = *currencyFromRow(row)
	}
	return result, nil
}

// GetCurrency retrieves a currency by uid.
func (s *CurrencyStore) GetCurrency(ctx context.Context, uid string) (*core.Currency, error) {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetCurrency(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get currency %q: %w", uid, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get currency: %w", err)
	}
	return currencyFromRow(row), nil
}
