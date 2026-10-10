package core

import "context"

// ReservationReader reads one reservation without locking it. Its state is a
// snapshot, not permission to settle: Reserver rechecks state, expiry and amount
// under the reservation row lock. Kept separate from Reserver and QueryProvider
// so existing consumer implementations of those interfaces remain compatible.
type ReservationReader interface {
	GetReservation(ctx context.Context, uid string) (*Reservation, error)
}
