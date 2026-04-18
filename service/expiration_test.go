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

type mockExpiredDepositFinder struct {
	deposits []core.Deposit
}

func (m *mockExpiredDepositFinder) GetExpiredDeposits(_ context.Context, limit int) ([]core.Deposit, error) {
	if limit > len(m.deposits) {
		limit = len(m.deposits)
	}
	return m.deposits[:limit], nil
}

type mockDepositExpirer struct {
	expired []int64
}

func (m *mockDepositExpirer) ExpireDeposit(_ context.Context, id int64) error {
	m.expired = append(m.expired, id)
	return nil
}

type mockExpiredWithdrawalFinder struct {
	withdrawals []core.Withdrawal
}

func (m *mockExpiredWithdrawalFinder) GetExpiredWithdrawals(_ context.Context, limit int) ([]core.Withdrawal, error) {
	if limit > len(m.withdrawals) {
		limit = len(m.withdrawals)
	}
	return m.withdrawals[:limit], nil
}

type mockWithdrawalFailer struct {
	failed []int64
}

func (m *mockWithdrawalFailer) FailWithdraw(_ context.Context, id int64, _ string) error {
	m.failed = append(m.failed, id)
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

	svc := NewExpirationService(finder, releaser, nil, nil, nil, nil, engine)

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

	svc := NewExpirationService(finder, releaser, nil, nil, nil, nil, engine)

	count, err := svc.ExpireStaleReservations(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
	assert.Empty(t, releaser.released)
}

func TestExpirationService_ExpiredDeposits(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	finder := &mockExpiredDepositFinder{
		deposits: []core.Deposit{
			{ID: 10, AccountHolder: 100, Status: core.DepositStatusPending, ExpiresAt: &past},
		},
	}
	expirer := &mockDepositExpirer{}
	engine := core.NewEngine()

	svc := NewExpirationService(nil, nil, finder, expirer, nil, nil, engine)

	count, err := svc.ExpireStaleDeposits(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, []int64{10}, expirer.expired)
}

func TestExpirationService_ExpiredWithdrawals(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	finder := &mockExpiredWithdrawalFinder{
		withdrawals: []core.Withdrawal{
			{ID: 20, AccountHolder: 100, Status: core.WithdrawStatusProcessing, ExpiresAt: &past},
		},
	}
	failer := &mockWithdrawalFailer{}
	engine := core.NewEngine()

	svc := NewExpirationService(nil, nil, nil, nil, finder, failer, engine)

	count, err := svc.ExpireStaleWithdrawals(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, []int64{20}, failer.failed)
}
