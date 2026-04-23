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
	reservationFinder  ExpiredReservationFinder
	reservationRelease ReservationReleaser
	bookingFinder      ExpiredBookingFinder
	bookingTransit     BookingTransitioner
	logger             core.Logger
	metrics            core.Metrics
}

// NewExpirationService creates a new ExpirationService.
func NewExpirationService(
	reservationFinder ExpiredReservationFinder,
	reservationRelease ReservationReleaser,
	bookingFinder ExpiredBookingFinder,
	bookingTransit BookingTransitioner,
	engine *core.Engine,
) *ExpirationService {
	return &ExpirationService{
		reservationFinder:  reservationFinder,
		reservationRelease: reservationRelease,
		bookingFinder:      bookingFinder,
		bookingTransit:     bookingTransit,
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
			BookingID: b.ID,
			ToStatus:  "expired",
		})
		if err != nil {
			s.logger.Error("service: expiration: expire booking failed",
				"booking_id", b.ID,
				"error", err,
			)
			continue
		}
		expired++
	}

	return expired, nil
}
