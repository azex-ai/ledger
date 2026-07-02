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
	released           []int64
	failIDs            map[int64]bool
	invalidTransitions map[int64]bool
}

func (m *mockReservationReleaser) Release(_ context.Context, id int64) error {
	if m.invalidTransitions != nil && m.invalidTransitions[id] {
		return fmt.Errorf("postgres: release: from %q to released: %w", "settled", core.ErrInvalidTransition)
	}
	if m.failIDs != nil && m.failIDs[id] {
		return fmt.Errorf("release failed for %d", id)
	}
	m.released = append(m.released, id)
	return nil
}

type mockReservationFinalizer struct {
	finalized          []int64
	failIDs            map[int64]bool
	invalidTransitions map[int64]bool
}

func (m *mockReservationFinalizer) FinalizeSettlement(_ context.Context, id int64) error {
	if m.invalidTransitions != nil && m.invalidTransitions[id] {
		return fmt.Errorf("postgres: finalize settlement: reservation %d has status %q, not settling: %w", id, "active", core.ErrInvalidTransition)
	}
	if m.failIDs != nil && m.failIDs[id] {
		return fmt.Errorf("finalize failed for %d", id)
	}
	m.finalized = append(m.finalized, id)
	return nil
}

// recordingLogger captures the level of each log call for assertions.
type recordingLogger struct {
	infoCalls  []string
	errorCalls []string
}

func (l *recordingLogger) Info(msg string, _ ...any)  { l.infoCalls = append(l.infoCalls, msg) }
func (l *recordingLogger) Warn(string, ...any)        {}
func (l *recordingLogger) Error(msg string, _ ...any) { l.errorCalls = append(l.errorCalls, msg) }

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

	svc := NewExpirationService(finder, releaser, &mockReservationFinalizer{}, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, []int64{1, 2}, releaser.released)
}

// TestExpirationService_ConcurrentReleaseRaceLoggedAsInfo verifies that a
// core.ErrInvalidTransition from Release — expected when another replica (or
// a racing settle/release call) already transitioned the reservation between
// the scan and this call — is logged at Info, not Error, while a genuine
// failure still logs at Error.
func TestExpirationService_ConcurrentReleaseRaceLoggedAsInfo(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	finder := &mockExpiredReservationFinder{
		reservations: []core.Reservation{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(50), Status: core.ReservationStatusActive, ExpiresAt: past},
			{ID: 2, AccountHolder: 200, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(75), Status: core.ReservationStatusActive, ExpiresAt: past},
		},
	}
	releaser := &mockReservationReleaser{
		invalidTransitions: map[int64]bool{1: true},
		failIDs:            map[int64]bool{2: true},
	}
	logger := &recordingLogger{}
	engine := core.NewEngine(core.WithLogger(logger))

	svc := NewExpirationService(finder, releaser, &mockReservationFinalizer{}, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, releaser.released)

	assert.Len(t, logger.infoCalls, 1, "the concurrent-transition race must log at Info")
	assert.Len(t, logger.errorCalls, 1, "the genuine failure must still log at Error")
}

// TestExpirationService_SettlingReservationsAreFinalized verifies that an
// expired reservation in the settling status (partially settled via
// SettlePartial) is finalized, not released — releasing it would discard the
// caller's partial settlement bookkeeping.
func TestExpirationService_SettlingReservationsAreFinalized(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	finder := &mockExpiredReservationFinder{
		reservations: []core.Reservation{
			{ID: 1, AccountHolder: 100, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(50), Status: core.ReservationStatusActive, ExpiresAt: past},
			{ID: 2, AccountHolder: 200, CurrencyID: 1, ReservedAmount: decimal.NewFromInt(75), Status: core.ReservationStatusSettling, ExpiresAt: past},
		},
	}
	releaser := &mockReservationReleaser{}
	finalizer := &mockReservationFinalizer{}
	engine := core.NewEngine()

	svc := NewExpirationService(finder, releaser, finalizer, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 2, count)
	assert.Equal(t, []int64{1}, releaser.released, "only the active reservation should be released")
	assert.Equal(t, []int64{2}, finalizer.finalized, "only the settling reservation should be finalized")
}

func TestExpirationService_NonExpiredUntouched(t *testing.T) {
	// No expired reservations
	finder := &mockExpiredReservationFinder{reservations: nil}
	releaser := &mockReservationReleaser{}
	engine := core.NewEngine()

	svc := NewExpirationService(finder, releaser, &mockReservationFinalizer{}, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, releaser.released)
}
