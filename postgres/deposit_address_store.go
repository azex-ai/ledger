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

var _ core.AddressRegistry = (*DepositAddressStore)(nil)

// DepositAddressStore implements core.AddressRegistry using PostgreSQL
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §2). Every write and
// lookup normalizes its address argument through core.ChecksumAddress first
// -- deposit_addresses' unique index is a plain case-sensitive btree
// (Foundation contract §7-2), so skipping normalization here would silently
// miss the index instead of erroring.
type DepositAddressStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
}

// NewDepositAddressStore creates a DepositAddressStore backed by a connection pool.
func NewDepositAddressStore(pool *pgxpool.Pool) *DepositAddressStore {
	return &DepositAddressStore{pool: pool, db: pool, q: sqlcgen.New(pool)}
}

// WithDB returns a clone of the DepositAddressStore bound to an existing transaction.
func (s *DepositAddressStore) WithDB(db DBTX) *DepositAddressStore {
	return &DepositAddressStore{pool: nil, db: db, q: sqlcgen.New(db)}
}

// EnsureAddress upserts input (see core.AddressRegistry.EnsureAddress for the
// upsert-by-holder / no-mismatch-reconciliation contract).
func (s *DepositAddressStore) EnsureAddress(ctx context.Context, input core.AddressRegistrationInput) (*core.DepositAddress, error) {
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("postgres: ensure deposit address: %w", err)
	}
	addr, err := core.ChecksumAddress(input.Address)
	if err != nil {
		return nil, fmt.Errorf("postgres: ensure deposit address: %w", err)
	}
	row, err := s.q.UpsertDepositAddress(ctx, sqlcgen.UpsertDepositAddressParams{
		Uid:           newUID(),
		AccountHolder: input.AccountHolder,
		Address:       addr,
		Factory:       input.Factory,
		InitHash:      input.InitHash,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: ensure deposit address", err)
	}
	return depositAddressFromRow(row), nil
}

// GetByAddress reverse-looks-up the holder for an observed on-chain address.
// address is checksum-normalized before querying -- callers may pass it in
// whatever casing the observation source produced.
func (s *DepositAddressStore) GetByAddress(ctx context.Context, address string) (*core.DepositAddress, error) {
	addr, err := core.ChecksumAddress(address)
	if err != nil {
		return nil, fmt.Errorf("postgres: get deposit address %q: %w", address, err)
	}
	row, err := s.q.GetDepositAddressByAddress(ctx, addr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get deposit address %q: %w", address, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get deposit address %q: %w", address, err)
	}
	return depositAddressFromRow(row), nil
}

// ListAddresses returns every registered deposit address.
func (s *DepositAddressStore) ListAddresses(ctx context.Context) ([]core.DepositAddress, error) {
	rows, err := s.q.ListDepositAddresses(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: list deposit addresses: %w", err)
	}
	out := make([]core.DepositAddress, len(rows))
	for i, row := range rows {
		out[i] = *depositAddressFromRow(row)
	}
	return out, nil
}

func depositAddressFromRow(row sqlcgen.DepositAddress) *core.DepositAddress {
	return &core.DepositAddress{
		UID:           pgToUID(row.Uid),
		AccountHolder: row.AccountHolder,
		Address:       row.Address,
		Factory:       row.Factory,
		InitHash:      row.InitHash,
		CreatedAt:     row.CreatedAt,
	}
}
