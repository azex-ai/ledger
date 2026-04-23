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

// ExpiredOperationFinder finds expired active operations.
type ExpiredOperationFinder interface {
	ListExpiredOperations(ctx context.Context, limit int) ([]core.Operation, error)
}

// OperationTransitioner transitions an operation's status.
type OperationTransitioner interface {
	Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error)
}

// ExpirationService cleans up stale reservations and operations.
type ExpirationService struct {
	reservationFinder  ExpiredReservationFinder
	reservationRelease ReservationReleaser
	operationFinder    ExpiredOperationFinder
	operationTransit   OperationTransitioner
	logger             core.Logger
	metrics            core.Metrics
}

// NewExpirationService creates a new ExpirationService.
func NewExpirationService(
	reservationFinder ExpiredReservationFinder,
	reservationRelease ReservationReleaser,
	operationFinder ExpiredOperationFinder,
	operationTransit OperationTransitioner,
	engine *core.Engine,
) *ExpirationService {
	return &ExpirationService{
		reservationFinder:  reservationFinder,
		reservationRelease: reservationRelease,
		operationFinder:    operationFinder,
		operationTransit:   operationTransit,
		logger:             engine.Logger(),
		metrics:            engine.Metrics(),
	}
}

// ExpireStaleReservations finds and releases expired active reservations.
func (s *ExpirationService) ExpireStaleReservations(ctx context.Context, batchSize int) (int, error) {
	if s.reservationFinder == nil {
		return 0, nil
	}
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
	}

	return released, nil
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
	}

	return expired, nil
}
