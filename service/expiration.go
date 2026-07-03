package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// ExpiredReservationFinder finds expired active reservations.
type ExpiredReservationFinder interface {
	GetExpiredReservations(ctx context.Context, limit int) ([]core.Reservation, error)
}

// ReservationReleaser releases a reservation by uid.
type ReservationReleaser interface {
	Release(ctx context.Context, reservationUID string) error
}

// ReservationFinalizer completes a partially-settled (settling) reservation.
type ReservationFinalizer interface {
	FinalizeSettlement(ctx context.Context, reservationUID string) error
}

// ExpiredBookingFinder finds expired active bookings.
type ExpiredBookingFinder interface {
	ListExpiredBookings(ctx context.Context, limit int) ([]core.Booking, error)
}

// BookingTransitioner transitions a booking's status.
type BookingTransitioner interface {
	Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error)
}

// ExpirationService cleans up stale reservations and bookings.
type ExpirationService struct {
	reservationFinder   ExpiredReservationFinder
	reservationRelease  ReservationReleaser
	reservationFinalize ReservationFinalizer
	bookingFinder       ExpiredBookingFinder
	bookingTransit      BookingTransitioner
	logger              core.Logger
	metrics             core.Metrics
}

// NewExpirationService creates a new ExpirationService.
func NewExpirationService(
	reservationFinder ExpiredReservationFinder,
	reservationRelease ReservationReleaser,
	reservationFinalize ReservationFinalizer,
	bookingFinder ExpiredBookingFinder,
	bookingTransit BookingTransitioner,
	engine *core.Engine,
) *ExpirationService {
	return &ExpirationService{
		reservationFinder:   reservationFinder,
		reservationRelease:  reservationRelease,
		reservationFinalize: reservationFinalize,
		bookingFinder:       bookingFinder,
		bookingTransit:      bookingTransit,
		logger:              engine.Logger(),
		metrics:             engine.Metrics(),
	}
}

// ExpireStaleReservations finds and winds down expired reservations: active
// ones are released in full, while settling ones (partially settled via
// SettlePartial) are finalized instead — keeping the settled portion and
// implicitly releasing the rest, rather than losing the partial settlement.
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
		var opErr error
		var opName string
		if r.Status == core.ReservationStatusSettling {
			opErr = s.reservationFinalize.FinalizeSettlement(ctx, r.UID)
			opName = "finalize settlement"
		} else {
			opErr = s.reservationRelease.Release(ctx, r.UID)
			opName = "release reservation"
		}
		if opErr != nil {
			if errors.Is(opErr, core.ErrInvalidTransition) {
				// Expected under multi-replica scanning: another replica (or a
				// racing settle/release call) already transitioned this
				// reservation between our scan and this call. Not a real
				// failure — just log for visibility.
				s.logger.Info("service: expiration: reservation already transitioned by a concurrent scan",
					"reservation_id", r.UID,
					"op", opName,
					"error", opErr,
				)
			} else {
				s.logger.Error("service: expiration: "+opName+" failed",
					"reservation_id", r.UID,
					"error", opErr,
				)
			}
			continue
		}
		released++
	}

	return released, nil
}

// ExpireStaleBookings finds and expires stale bookings via state transition.
func (s *ExpirationService) ExpireStaleBookings(ctx context.Context, batchSize int) (int, error) {
	if s.bookingFinder == nil {
		return 0, nil
	}

	bookings, err := s.bookingFinder.ListExpiredBookings(ctx, batchSize)
	if err != nil {
		return 0, fmt.Errorf("service: expiration: find expired bookings: %w", err)
	}

	expired := 0
	for _, b := range bookings {
		_, err := s.bookingTransit.Transition(ctx, core.TransitionInput{
			BookingUID: b.UID,
			ToStatus:   "expired",
		})
		if err != nil {
			s.logger.Error("service: expiration: expire booking failed",
				"booking_id", b.UID,
				"error", err,
			)
			continue
		}
		expired++
	}

	return expired, nil
}
