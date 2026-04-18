package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.Reserver = (*ReserverStore)(nil)

// ReserverStore implements core.Reserver using PostgreSQL with advisory locks.
type ReserverStore struct {
	pool   *pgxpool.Pool
	q      *sqlcgen.Queries
	ledger *LedgerStore
}

// NewReserverStore creates a new ReserverStore.
func NewReserverStore(pool *pgxpool.Pool, ledger *LedgerStore) *ReserverStore {
	return &ReserverStore{
		pool:   pool,
		q:      sqlcgen.New(pool),
		ledger: ledger,
	}
}

// Reserve creates an amount reservation with advisory lock serialization.
// Idempotent: returns existing reservation if idempotency_key matches.
func (s *ReserverStore) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	if !input.Amount.IsPositive() {
		return nil, fmt.Errorf("postgres: reserve: amount must be positive")
	}

	// Check idempotency first (outside tx)
	existing, err := s.q.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return reservationFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Advisory lock on account_holder to serialize concurrent reserves
	_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", input.AccountHolder)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: advisory lock: %w", err)
	}

	// Double-check idempotency inside lock
	qtx := s.q.WithTx(tx)
	existing, err = qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("postgres: reserve: commit idempotent: %w", err)
		}
		return reservationFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency in tx: %w", err)
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
		return nil, fmt.Errorf("postgres: reserve: insert: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reserve: commit: %w", err)
	}

	return reservationFromRow(row), nil
}

// Settle marks a reservation as settled with the actual amount.
func (s *ReserverStore) Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: settle: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	res, err := qtx.GetReservationForUpdate(ctx, reservationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: settle: reservation %d not found", reservationID)
		}
		return fmt.Errorf("postgres: settle: get reservation: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if !status.CanTransitionTo(core.ReservationStatusSettled) {
		return fmt.Errorf("postgres: settle: invalid transition from %q to settled", res.Status)
	}

	if err := qtx.UpdateReservationSettle(ctx, sqlcgen.UpdateReservationSettleParams{
		ID:            reservationID,
		SettledAmount: decimalToNumeric(actualAmount),
		JournalID:     pgtype.Int8{Valid: false},
	}); err != nil {
		return fmt.Errorf("postgres: settle: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: settle: commit: %w", err)
	}

	return nil
}

// Release cancels an active reservation.
func (s *ReserverStore) Release(ctx context.Context, reservationID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: release: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	res, err := qtx.GetReservationForUpdate(ctx, reservationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: release: reservation %d not found", reservationID)
		}
		return fmt.Errorf("postgres: release: get reservation: %w", err)
	}

	status := core.ReservationStatus(res.Status)
	if !status.CanTransitionTo(core.ReservationStatusReleased) {
		return fmt.Errorf("postgres: release: invalid transition from %q to released", res.Status)
	}

	if err := qtx.UpdateReservationStatus(ctx, sqlcgen.UpdateReservationStatusParams{
		ID:     reservationID,
		Status: string(core.ReservationStatusReleased),
	}); err != nil {
		return fmt.Errorf("postgres: release: update: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: release: commit: %w", err)
	}

	return nil
}
