package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"go.opentelemetry.io/otel/attribute"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
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
// Idempotent: same key + same payload returns the existing reservation;
// divergent payload returns ErrConflict.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the reservation is written into the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Reserve(ctx context.Context, input core.ReserveInput) (*core.Reservation, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.reserve",
		attribute.Int64("account_holder", input.AccountHolder),
		attribute.Int64("currency_id", input.CurrencyID),
		attribute.String("idempotency_key", input.IdempotencyKey),
		attribute.String("amount", input.Amount.String()),
	)
	defer span.End()

	if err := input.Validate(); err != nil {
		err := fmt.Errorf("postgres: reserve: %w", err)
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	// Check idempotency first (outside tx / on the current db handle).
	existing, err := s.q.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return ensureReservationMatchesInput(existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: check idempotency: %w", err)
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		res, err := s.reserveWithQueries(ctx, s.q, input)
		ledgerotel.RecordError(span, err)
		return res, err
	}

	// Pool mode: own the transaction lifecycle.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	res, err := s.reserveWithQueries(ctx, qtx, input)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return nil, fmt.Errorf("postgres: reserve: commit: %w", err)
	}

	return res, nil
}

func (s *ReserverStore) reserveWithQueries(ctx context.Context, qtx *sqlcgen.Queries, input core.ReserveInput) (*core.Reservation, error) {
	if err := acquireIdempotencyLock(ctx, qtx, input.IdempotencyKey); err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}

	existing, err := qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return ensureReservationMatchesInput(existing, input)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reserve: check idempotency in tx: %w", err)
	}

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

	// No second idempotency recheck needed: the advisory lock from
	// acquireIdempotencyLock above already serializes all same-key transactions,
	// so nothing could have inserted a matching row between then and now.

	// Account policy enforcement (I-17): Reserve is unconditionally a
	// consumption entry point (it locks funds toward future spend), so
	// frozen/closed both reject it outright — no direction/netting question
	// applies here the way it does for PostJournal entries. classificationID
	// is 0 because a reservation isn't tied to any classification; only the
	// (holder,currency,0) and (holder,0,0) policy tiers can ever match.
	// Evaluated inside the same advisory lock as the balance check below, so
	// it is TOCTOU-safe against a concurrent SetPolicy on the same pair.
	policy, err := getEffectiveAccountPolicy(ctx, qtx, input.AccountHolder, input.CurrencyID, 0)
	if err != nil {
		return nil, fmt.Errorf("postgres: reserve: %w", err)
	}
	if policy != nil {
		switch policy.Status {
		case core.AccountPolicyStatusClosed:
			return nil, fmt.Errorf("postgres: reserve: account %d currency %d is closed (policy %d): %w", input.AccountHolder, input.CurrencyID, policy.ID, core.ErrAccountClosed)
		case core.AccountPolicyStatusFrozen:
			return nil, fmt.Errorf("postgres: reserve: account %d currency %d is frozen (policy %d): %w", input.AccountHolder, input.CurrencyID, policy.ID, core.ErrAccountFrozen)
		}
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

	expiresAt := time.Now().Add(resolveReservationExpiresIn(input.ExpiresIn))

	row, err := qtx.InsertReservation(ctx, sqlcgen.InsertReservationParams{
		AccountHolder:  input.AccountHolder,
		CurrencyID:     input.CurrencyID,
		ReservedAmount: decimalToNumeric(input.Amount),
		IdempotencyKey: input.IdempotencyKey,
		ExpiresAt:      expiresAt,
	})
	if err != nil {
		existing, lookupErr := qtx.GetReservationByIdempotencyKey(ctx, input.IdempotencyKey)
		if lookupErr == nil {
			return ensureReservationMatchesInput(existing, input)
		}
		if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reserve: insert: %w (idempotency recheck: %v)", normalizeStoreError(err), lookupErr)
		}
		return nil, wrapStoreError("postgres: reserve: insert", err)
	}

	return reservationFromRow(row), nil
}

// reservationDefaultExpiresIn is applied when ReserveInput.ExpiresIn is zero.
const reservationDefaultExpiresIn = 15 * time.Minute

// resolveReservationExpiresIn returns the duration that will be added to
// time.Now() when storing ExpiresAt. Both the insert path and the idempotency
// match path use it so retries with the same input compare equal.
func resolveReservationExpiresIn(d time.Duration) time.Duration {
	if d == 0 {
		return reservationDefaultExpiresIn
	}
	return d
}

// Settle marks a reservation as settled with the actual amount.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Settle(ctx context.Context, reservationID int64, actualAmount decimal.Decimal) error {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.settle",
		attribute.Int64("reservation_id", reservationID),
		attribute.String("actual_amount", actualAmount.String()),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		err := s.settleWithQueries(ctx, s.q, reservationID, actualAmount)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.settleWithQueries(ctx, s.q.WithTx(tx), reservationID, actualAmount); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: settle: commit: %w", err)
	}

	return nil
}

func (s *ReserverStore) settleWithQueries(ctx context.Context, qtx *sqlcgen.Queries, reservationID int64, actualAmount decimal.Decimal) error {
	if !actualAmount.IsPositive() {
		return fmt.Errorf("postgres: settle: actual amount must be positive, got %s: %w", actualAmount, core.ErrInvalidInput)
	}

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

	// The reservations table enforces settled_amount <= reserved_amount via
	// chk_settled_lte_reserved, but check here too so callers get a clear
	// core.ErrInvalidInput without a round trip to the DB constraint.
	reservedAmount, err := numericToDecimal(res.ReservedAmount)
	if err != nil {
		return fmt.Errorf("postgres: settle: convert reserved amount: %w", err)
	}
	if actualAmount.GreaterThan(reservedAmount) {
		return fmt.Errorf("postgres: settle: actual amount %s exceeds reserved amount %s: %w", actualAmount, reservedAmount, core.ErrInvalidInput)
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

// HeldAmount returns the sum of reserved_amount across the holder's active
// reservations in the given currency. This is the same figure Reserve subtracts
// from balance when checking availability, exposed so consumers can compute
// available = balance − held without reaching into the reservations table.
func (s *ReserverStore) HeldAmount(ctx context.Context, holder, currencyID int64) (decimal.Decimal, error) {
	total, err := s.q.SumActiveReservations(ctx, sqlcgen.SumActiveReservationsParams{
		AccountHolder: holder,
		CurrencyID:    currencyID,
	})
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: held amount: %w", err)
	}
	held, err := anyToDecimal(total)
	if err != nil {
		return decimal.Zero, fmt.Errorf("postgres: held amount: convert: %w", err)
	}
	return held, nil
}

// Release cancels an active reservation.
//
// In pool mode a new transaction is started and committed here.
// In tx mode (bound via withDB) the update is applied to the caller's
// transaction; commit/rollback is the caller's responsibility.
func (s *ReserverStore) Release(ctx context.Context, reservationID int64) error {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.reserver.release",
		attribute.Int64("reservation_id", reservationID),
	)
	defer span.End()

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		err := s.releaseWithQueries(ctx, s.q, reservationID)
		ledgerotel.RecordError(span, err)
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		ledgerotel.RecordError(span, err)
		return fmt.Errorf("postgres: release: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if err := s.releaseWithQueries(ctx, s.q.WithTx(tx), reservationID); err != nil {
		ledgerotel.RecordError(span, err)
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		ledgerotel.RecordError(span, err)
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
