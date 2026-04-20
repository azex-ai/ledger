package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.Depositor = (*DepositStore)(nil)

// DepositStore implements core.Depositor using PostgreSQL.
type DepositStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewDepositStore creates a new DepositStore.
func NewDepositStore(pool *pgxpool.Pool) *DepositStore {
	return &DepositStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// InitDeposit creates a new deposit in pending status.
func (s *DepositStore) InitDeposit(ctx context.Context, input core.DepositInput) (*core.Deposit, error) {
	// Check idempotency: return existing deposit if key matches
	existing, err := s.q.GetDepositByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return depositFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: init deposit: check idempotency: %w", err)
	}

	row, err := s.q.InsertDeposit(ctx, sqlcgen.InsertDepositParams{
		AccountHolder:  input.AccountHolder,
		CurrencyID:     input.CurrencyID,
		ExpectedAmount: decimalToNumeric(input.ExpectedAmount),
		ChannelName:    input.ChannelName,
		IdempotencyKey: input.IdempotencyKey,
		Metadata:       metadataToJSON(input.Metadata),
		ExpiresAt:      timePtrToTimestamptz(input.ExpiresAt),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: init deposit: %w", err)
	}
	return depositFromRow(row), nil
}

// ConfirmingDeposit moves a deposit to confirming status with a channel reference.
func (s *DepositStore) ConfirmingDeposit(ctx context.Context, depositID int64, channelRef string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: confirming deposit: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	dep, err := qtx.GetDepositForUpdate(ctx, depositID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: confirming deposit: deposit %d: %w", depositID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: confirming deposit: get: %w", err)
	}

	status := core.DepositStatus(dep.Status)
	if !status.CanTransitionTo(core.DepositStatusConfirming) {
		return fmt.Errorf("postgres: confirming deposit: from %q to confirming: %w", dep.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateDepositConfirming(ctx, sqlcgen.UpdateDepositConfirmingParams{
		ID:         depositID,
		ChannelRef: pgtype.Text{String: channelRef, Valid: true},
	}); err != nil {
		return fmt.Errorf("postgres: confirming deposit: update: %w", err)
	}

	return tx.Commit(ctx)
}

// ConfirmDeposit finalizes a deposit with actual amount and journal reference.
func (s *DepositStore) ConfirmDeposit(ctx context.Context, input core.ConfirmDepositInput) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: confirm deposit: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	dep, err := qtx.GetDepositForUpdate(ctx, input.DepositID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: confirm deposit: deposit %d: %w", input.DepositID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: confirm deposit: get: %w", err)
	}

	// If already confirmed, this is idempotent
	if core.DepositStatus(dep.Status) == core.DepositStatusConfirmed {
		return tx.Commit(ctx)
	}

	status := core.DepositStatus(dep.Status)
	if !status.CanTransitionTo(core.DepositStatusConfirmed) {
		return fmt.Errorf("postgres: confirm deposit: from %q to confirmed: %w", dep.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateDepositConfirm(ctx, sqlcgen.UpdateDepositConfirmParams{
		ID:           input.DepositID,
		ActualAmount: decimalToNumeric(input.ActualAmount),
		ChannelRef:   pgtype.Text{String: input.ChannelRef, Valid: true},
		JournalID:    pgtype.Int8{Valid: false}, // Journal linked by service layer
	}); err != nil {
		return fmt.Errorf("postgres: confirm deposit: update: %w", err)
	}

	return tx.Commit(ctx)
}

// FailDeposit marks a deposit as failed.
func (s *DepositStore) FailDeposit(ctx context.Context, depositID int64, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: fail deposit: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	dep, err := qtx.GetDepositForUpdate(ctx, depositID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: fail deposit: deposit %d: %w", depositID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: fail deposit: get: %w", err)
	}

	status := core.DepositStatus(dep.Status)
	if !status.CanTransitionTo(core.DepositStatusFailed) {
		return fmt.Errorf("postgres: fail deposit: from %q to failed: %w", dep.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateDepositStatus(ctx, sqlcgen.UpdateDepositStatusParams{
		ID:     depositID,
		Status: string(core.DepositStatusFailed),
	}); err != nil {
		return fmt.Errorf("postgres: fail deposit: update: %w", err)
	}

	return tx.Commit(ctx)
}

// ExpireDeposit marks a deposit as expired.
func (s *DepositStore) ExpireDeposit(ctx context.Context, depositID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: expire deposit: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	dep, err := qtx.GetDepositForUpdate(ctx, depositID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: expire deposit: deposit %d: %w", depositID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: expire deposit: get: %w", err)
	}

	status := core.DepositStatus(dep.Status)
	if !status.CanTransitionTo(core.DepositStatusExpired) {
		return fmt.Errorf("postgres: expire deposit: from %q to expired: %w", dep.Status, core.ErrInvalidTransition)
	}

	if err := qtx.UpdateDepositStatus(ctx, sqlcgen.UpdateDepositStatusParams{
		ID:     depositID,
		Status: string(core.DepositStatusExpired),
	}); err != nil {
		return fmt.Errorf("postgres: expire deposit: update: %w", err)
	}

	return tx.Commit(ctx)
}
