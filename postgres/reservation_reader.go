package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/azex-ai/ledger/core"
)

var _ core.ReservationReader = (*ReserverStore)(nil)

// GetReservation reads by UID through this store's pool or caller transaction.
// It deliberately takes no row lock: composed money operations first acquire
// their complete advisory-lock set, then settlement locks and rechecks the row.
func (s *ReserverStore) GetReservation(ctx context.Context, uid string) (*core.Reservation, error) {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetReservationByUID(ctx, pgUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: get reservation %q: %w", uid, core.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get reservation: %w", err)
	}
	return reservationFromRow(ctx, s.dims, s.q, row)
}
