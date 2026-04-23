package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// --- Mocks ---

type mockExpiredReservationFinder struct {
	reservations []core.Reservation
}

func (m *mockExpiredReservationFinder) GetExpiredReservations(_ context.Context, limit int) ([]core.Reservation, error) {
	if limit > len(m.reservations) {
		limit = len(m.reservations)
	}
	return m.reservations[:limit], nil
}

type mockReservationReleaser struct {
	released []int64
	failIDs  map[int64]bool
}

func (m *mockReservationReleaser) Release(_ context.Context, id int64) error {
	if m.failIDs != nil && m.failIDs[id] {
		return fmt.Errorf("release failed for %d", id)
	}
	m.released = append(m.released, id)
	return nil
}

// --- Tests ---

func TestExpirationService_ExpiredReservations(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	finder := &mockExpiredReservationFinder{
		reservations: []core.Reservation{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(50), Status: core.ReservationStatusActive, ExpiresAt: past},
			{ID: 2, AccountHolder: 200, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(75), Status: core.ReservationStatusActive, ExpiresAt: past},
		},
	}
	releaser := &mockReservationReleaser{}
	engine := core.NewEngine()

	svc := NewExpirationService(finder, releaser, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, []int64{1, 2}, releaser.released)
}

func TestExpirationService_NonExpiredUntouched(t *testing.T) {
	// No expired reservations
	finder := &mockExpiredReservationFinder{reservations: nil}
	releaser := &mockReservationReleaser{}
	engine := core.NewEngine()

	svc := NewExpirationService(finder, releaser, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, releaser.released)
}
