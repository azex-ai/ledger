package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestReservationStatus_IsValid(t *testing.T) {
	assert.True(t, ReservationStatusActive.IsValid())
	assert.True(t, ReservationStatusSettling.IsValid())
	assert.True(t, ReservationStatusSettled.IsValid())
	assert.True(t, ReservationStatusReleased.IsValid())
	assert.False(t, ReservationStatus("bogus").IsValid())
}

func TestReservationStatus_CanTransition(t *testing.T) {
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusSettling))
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusSettled))
	assert.True(t, ReservationStatusActive.CanTransitionTo(ReservationStatusReleased))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusSettled))
	assert.True(t, ReservationStatusSettling.CanTransitionTo(ReservationStatusReleased))
	assert.False(t, ReservationStatusSettled.CanTransitionTo(ReservationStatusActive))
	assert.False(t, ReservationStatusReleased.CanTransitionTo(ReservationStatusActive))
}
