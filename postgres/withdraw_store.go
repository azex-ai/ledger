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

// Note: core.Withdrawer was removed in v2 refactor. WithdrawStore is legacy code.
// var _ core.Withdrawer = (*WithdrawStore)(nil)

// WithdrawStore implements core.Withdrawer using PostgreSQL.
type WithdrawStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewWithdrawStore creates a new WithdrawStore.
func NewWithdrawStore(pool *pgxpool.Pool) *WithdrawStore {
	return &WithdrawStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// InitWithdraw creates a new withdrawal in locked status.
func (s *WithdrawStore) InitWithdraw(ctx context.Context, input core.WithdrawInput) (*core.Withdrawal, error) {
	// Check idempotency: return existing withdrawal if key matches
	existing, err := s.q.GetWithdrawalByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return withdrawalFromRow(existing), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: init withdraw: check idempotency: %w", err)
	}

	row, err := s.q.InsertWithdrawal(ctx, sqlcgen.InsertWithdrawalParams{
		AccountHolder:  input.AccountHolder,
		CurrencyID:     input.CurrencyID,
		Amount:         decimalToNumeric(input.Amount),
		ChannelName:    input.ChannelName,
		IdempotencyKey: input.IdempotencyKey,
		Metadata:       metadataToJSON(input.Metadata),
		ReviewRequired: input.ReviewRequired,
		ExpiresAt:      timePtrToTimestamptz(input.ExpiresAt),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: init withdraw: %w", err)
	}
	return withdrawalFromRow(row), nil
}

// ReserveWithdraw transitions a withdrawal from locked to reserved.
func (s *WithdrawStore) ReserveWithdraw(ctx context.Context, withdrawalID int64) error {
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusReserved, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
			ID:     withdrawalID,
			Status: string(core.WithdrawStatusReserved),
		})
	})
}

// ReviewWithdraw approves or rejects a withdrawal under review.
func (s *WithdrawStore) ReviewWithdraw(ctx context.Context, withdrawalID int64, approved bool) error {
	if approved {
		// reviewing -> processing
		return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusProcessing, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
			return qtx.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
				ID:     withdrawalID,
				Status: string(core.WithdrawStatusProcessing),
			})
		})
	}
	// reviewing -> failed
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusFailed, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
			ID:     withdrawalID,
			Status: string(core.WithdrawStatusFailed),
		})
	})
}

// ProcessWithdraw transitions a withdrawal to processing with a channel reference.
func (s *WithdrawStore) ProcessWithdraw(ctx context.Context, withdrawalID int64, channelRef string) error {
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusProcessing, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalProcess(ctx, sqlcgen.UpdateWithdrawalProcessParams{
			ID:         withdrawalID,
			ChannelRef: pgtype.Text{String: channelRef, Valid: true},
		})
	})
}

// ConfirmWithdraw finalizes a withdrawal.
func (s *WithdrawStore) ConfirmWithdraw(ctx context.Context, withdrawalID int64) error {
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusConfirmed, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalConfirm(ctx, sqlcgen.UpdateWithdrawalConfirmParams{
			ID:        withdrawalID,
			JournalID: pgtype.Int8{Valid: false},
		})
	})
}

// FailWithdraw marks a withdrawal as failed.
func (s *WithdrawStore) FailWithdraw(ctx context.Context, withdrawalID int64, reason string) error {
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusFailed, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
			ID:     withdrawalID,
			Status: string(core.WithdrawStatusFailed),
		})
	})
}

// RetryWithdraw transitions a failed withdrawal back to reserved for retry.
func (s *WithdrawStore) RetryWithdraw(ctx context.Context, withdrawalID int64) error {
	return s.transitionWithdrawal(ctx, withdrawalID, core.WithdrawStatusReserved, func(qtx *sqlcgen.Queries, _ sqlcgen.Withdrawal) error {
		return qtx.UpdateWithdrawalStatus(ctx, sqlcgen.UpdateWithdrawalStatusParams{
			ID:     withdrawalID,
			Status: string(core.WithdrawStatusReserved),
		})
	})
}

// transitionWithdrawal is a generic state machine transition helper.
func (s *WithdrawStore) transitionWithdrawal(
	ctx context.Context,
	withdrawalID int64,
	target core.WithdrawStatus,
	updateFn func(*sqlcgen.Queries, sqlcgen.Withdrawal) error,
) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: withdraw transition: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	w, err := qtx.GetWithdrawalForUpdate(ctx, withdrawalID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("postgres: withdraw transition: withdrawal %d: %w", withdrawalID, core.ErrNotFound)
		}
		return fmt.Errorf("postgres: withdraw transition: get: %w", err)
	}

	status := core.WithdrawStatus(w.Status)
	if !status.CanTransitionTo(target) {
		return fmt.Errorf("postgres: withdraw transition: from %q to %q: %w", w.Status, target, core.ErrInvalidTransition)
	}

	if err := updateFn(qtx, w); err != nil {
		return fmt.Errorf("postgres: withdraw transition: update: %w", err)
	}

	return tx.Commit(ctx)
}
