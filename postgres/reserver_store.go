package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.Reserver = (*ReserverStore)(nil)

// ReserverStore implements core.Reserver using PostgreSQL with advisory locks.
//
// In pool mode (constructed via NewReserverStore), each write operation starts
// its own transaction. In tx mode (bound via withDB), write operations
// participate in the caller's transaction — commit/rollback is the caller's
// responsibility.
type ReserverStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
	pool   *pgxpool.Pool
	db     DBTX
	q      *sqlcgen.Queries
	ledger *LedgerStore
}

// NewReserverStore creates a new ReserverStore backed by a connection pool.
func NewReserverStore(pool *pgxpool.Pool, ledger *LedgerStore) *ReserverStore {
	return &ReserverStore{
		pool:   pool,
		db:     pool,
		q:      sqlcgen.New(pool),
		ledger: ledger,
	}
}

// WithDB returns a clone of the ReserverStore bound to an existing transaction.
// ledger must be a LedgerStore already bound to the same transaction so that
// balance checks and advisory locks share the same connection.
func (s *ReserverStore) WithDB(db DBTX, ledger *LedgerStore) *ReserverStore {
	return &ReserverStore{
		pool:   nil, // tx mode
		db:     db,
		q:      sqlcgen.New(db),
		ledger: ledger,
	}
}

// Reserve creates an amount reservation with advisory lock serialization.
// Idempotent: returns existing reservation if idempotency_key matches.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the reservation is written into the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	if !input.Amount.IsPositive() {
		return nil, fmt.Errorf("postgres: reserve: amount must be positive: %w", core.ErrInvalidInput)
	}

	// Check idempotency first (outside tx / on the current db handle).
	existing, err := s.q.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return reservationFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency: %w", err)
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		return s.reserveWithQueries(ctx, s.q, input)
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	res, err := s.reserveWithQueries(ctx, qtx, input)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reserve: commit: %w", err)
	}

	return res, nil
}

func (s *ReserverStore) reserveWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.ReserveInput) (*core.Reservation, error) {
	// Invariant (matches LedgerStore.PostJournal): all balance-mutating tx must
	// take pg_advisory_xact_lock(holder, currency_id) for every affected pair,
	// in sorted order. Reserve only ever touches a single pair, but we still
	// route through the same helper so the lock space (two-arg int4 form) stays
	// consistent across reserve and post-journal.
	if err := acquireBalanceLocks(ctx, qtx, []balancePair{{
		holder:     input.AccountHolder,
		currencyID: input.CurrencyID,
	}}); err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}

	// Double-check idempotency inside lock.
	existing, err := qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return reservationFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency in tx: %w", err)
	}

	// Check sufficient balance before reserving.
	// The advisory lock above serializes concurrent reserves for the same (holder, currency),
	// so this read is safe against TOCTOU races.
	balances, err := s.ledger.GetBalances(ctx, input.AccountHolder, input.CurrencyID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: get balances: %w", err)
	}
	var totalBalance decimal.Decimal
	for _, b := range balances {
		totalBalance = totalBalance.Add(b.Balance)
	}

	activeReserved, err := qtx.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: input.AccountHolder,
		CurrencyID:    input.CurrencyID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: sum active reservations: %w", err)
	}
	activeReservedDecimal, err := anyToDecimal(activeReserved)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: convert active reservations: %w", err)
	}

	available := totalBalance.Sub(activeReservedDecimal)
	if available.LessThan(input.Amount) {
		return nil, fmt.Errorf("postgres: reserve: available %s < requested %s: %w", available.String(), input.Amount.String(), core.ErrInsufficientBalance)
	}

	expiresAt := time.Now().Add(input.ExpiresIn)
	if input.ExpiresIn == 0 {
		expiresAt = time.Now().Add(15 * time.Minute)
	}

	row, err := qtx.InsertReservation(ctx, sqlcgen.InsertReservationParams{
		AccountHolder:  input.AccountHolder,
		CurrencyID:     input.CurrencyID,
		ReservedAmount: decimalToNumeric(input.Amount),
		IdempotencyKey: input.IdempotencyKey,
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: reserve: insert", err)
	}

	return reservationFromRow(row), nil
}

// Settle marks a reservation as settled with the actual amount.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		return s.settleWithQueries(ctx, s.q, reservationID, actualAmount)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: settle: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.settleWithQueries(ctx, s.q.WithTx(tx), reservationID, actualAmount); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: settle: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) settleWithQueries(ctx context.Context, qtx *sqlcgen.Queries, reservationID int64, actualAmount decimal.Decimal) error {
	res, err := qtx.GetReservationForUpdate(ctx, reservationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: settle: reservation %d: %w", reservationID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: settle: get reservation: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if !status.CanTransitionTo(core.ReservationStatusSettled) {
		return fmt.Errorf("postgres: settle: from %q to settled: %w", res.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateReservationSettle(ctx, sqlcgen.UpdateReservationSettleParams{
		ID:            reservationID,
		SettledAmount: decimalToNumeric(actualAmount),
		JournalID:     0,
	}); err != nil {
		return wrapStoreError("postgres: settle: update", err)
	}

	return nil
}

// Release cancels an active reservation.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Release(ctx context.Context, reservationID int64) error {
	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		return s.releaseWithQueries(ctx, s.q, reservationID)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: release: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.releaseWithQueries(ctx, s.q.WithTx(tx), reservationID); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: release: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) releaseWithQueries(ctx context.Context, qtx *sqlcgen.Queries, reservationID int64) error {
	res, err := qtx.GetReservationForUpdate(ctx, reservationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: release: reservation %d: %w", reservationID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: release: get reservation: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if !status.CanTransitionTo(core.ReservationStatusReleased) {
		return fmt.Errorf("postgres: release: from %q to released: %w", res.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateReservationStatus(ctx, sqlcgen.UpdateReservationStatusParams{
		ID:     reservationID,
		Status: string(core.ReservationStatusReleased),
	}); err != nil {
		return wrapStoreError("postgres: release: update", err)
	}

	return nil
}
