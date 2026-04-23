package service

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// ExpiredReservationFinder finds expired active reservations.
type ExpiredReservationFinder interface {
	GetExpiredReservations(ctx context.Context, limit int) ([]core.Reservation, error)
}

// ReservationReleaser releases a reservation by ID.
type ReservationReleaser interface {
	Release(ctx context.Context, reservationID int64) error
}

// ExpiredDepositFinder finds expired pending/confirming deposits.
type ExpiredDepositFinder interface {
	GetExpiredDeposits(ctx context.Context, limit int) ([]core.Deposit, error)
}

// DepositExpirer expires a deposit by ID.
type DepositExpirer interface {
	ExpireDeposit(ctx context.Context, depositID int64) error
}

// ExpiredWithdrawalFinder finds expired processing withdrawals.
type ExpiredWithdrawalFinder interface {
	GetExpiredWithdrawals(ctx context.Context, limit int) ([]core.Withdrawal, error)
}

// WithdrawalFailer fails a withdrawal by ID.
type WithdrawalFailer interface {
	FailWithdraw(ctx context.Context, withdrawalID int64, reason string) error
}

// ExpiredOperationFinder finds expired active operations.
type ExpiredOperationFinder interface {
	ListExpiredOperations(ctx context.Context, limit int) ([]core.Operation, error)
}

// OperationTransitioner transitions an operation's status.
type OperationTransitioner interface {
	Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error)
}

// ExpirationService cleans up stale reservations, deposits, withdrawals, and operations.
type ExpirationService struct {
	reservationFinder  ExpiredReservationFinder
	reservationRelease ReservationReleaser
	depositFinder      ExpiredDepositFinder
	depositExpire      DepositExpirer
	withdrawalFinder   ExpiredWithdrawalFinder
	withdrawalFail     WithdrawalFailer
	operationFinder    ExpiredOperationFinder
	operationTransit   OperationTransitioner
	logger             core.Logger
	metrics            core.Metrics
}

// NewExpirationService creates a new ExpirationService.
func NewExpirationService(
	reservationFinder ExpiredReservationFinder,
	reservationRelease ReservationReleaser,
	depositFinder ExpiredDepositFinder,
	depositExpire DepositExpirer,
	withdrawalFinder ExpiredWithdrawalFinder,
	withdrawalFail WithdrawalFailer,
	operationFinder ExpiredOperationFinder,
	operationTransit OperationTransitioner,
	engine *core.Engine,
) *ExpirationService {
	return &ExpirationService{
		reservationFinder:  reservationFinder,
		reservationRelease: reservationRelease,
		depositFinder:      depositFinder,
		depositExpire:      depositExpire,
		withdrawalFinder:   withdrawalFinder,
		withdrawalFail:     withdrawalFail,
		operationFinder:    operationFinder,
		operationTransit:   operationTransit,
		logger:             engine.Logger(),
		metrics:            engine.Metrics(),
	}
}

// ExpireStaleReservations finds and releases expired active reservations.
func (s *ExpirationService) ExpireStaleReservations(ctx context.Context, batchSize int) (int, error) {
	reservations, err := s.reservationFinder.GetExpiredReservations(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: expiration: find expired reservations: %w", err)
	}

	released := 0
	for _, r := range reservations {
		if err := s.reservationRelease.Release(ctx, r.ID); err != nil {
			s.logger.Error("service: expiration: release reservation failed",
				"reservation_id", r.ID,
				"error", err,
			)
			continue
		}
		released++
		s.logger.Info("service: expiration: reservation released",
			"reservation_id", r.ID,
			"holder", r.AccountHolder,
		)
	}

	return released, nil
}

// ExpireStaleDeposits finds and expires stale pending/confirming deposits.
func (s *ExpirationService) ExpireStaleDeposits(ctx context.Context, batchSize int) (int, error) {
	deposits, err := s.depositFinder.GetExpiredDeposits(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: expiration: find expired deposits: %w", err)
	}

	expired := 0
	for _, d := range deposits {
		if err := s.depositExpire.ExpireDeposit(ctx, d.ID); err != nil {
			s.logger.Error("service: expiration: expire deposit failed",
				"deposit_id", d.ID,
				"error", err,
			)
			continue
		}
		expired++
		s.logger.Info("service: expiration: deposit expired",
			"deposit_id", d.ID,
			"holder", d.AccountHolder,
		)
	}

	return expired, nil
}

// ExpireStaleWithdrawals finds and fails stale processing withdrawals.
func (s *ExpirationService) ExpireStaleWithdrawals(ctx context.Context, batchSize int) (int, error) {
	withdrawals, err := s.withdrawalFinder.GetExpiredWithdrawals(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: expiration: find expired withdrawals: %w", err)
	}

	failed := 0
	for _, w := range withdrawals {
		if err := s.withdrawalFail.FailWithdraw(ctx, w.ID, "expired"); err != nil {
			s.logger.Error("service: expiration: fail withdrawal failed",
				"withdrawal_id", w.ID,
				"error", err,
			)
			continue
		}
		failed++
		s.logger.Info("service: expiration: withdrawal expired",
			"withdrawal_id", w.ID,
			"holder", w.AccountHolder,
		)
	}

	return failed, nil
}

// ExpireStaleOperations finds and expires stale operations via state transition.
func (s *ExpirationService) ExpireStaleOperations(ctx context.Context, batchSize int) (int, error) {
	if s.operationFinder == nil {
		return 0, nil
	}

	ops, err := s.operationFinder.ListExpiredOperations(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: expiration: find expired operations: %w", err)
	}

	expired := 0
	for _, op := range ops {
		_, err := s.operationTransit.Transition(ctx, core.TransitionInput{
			OperationID: op.ID,
			ToStatus:    "expired",
		})
		if err != nil {
			s.logger.Error("service: expiration: expire operation failed",
				"operation_id", op.ID,
				"error", err,
			)
			continue
		}
		expired++
		s.logger.Info("service: expiration: operation expired",
			"operation_id", op.ID,
			"holder", op.AccountHolder,
		)
	}

	return expired, nil
}
