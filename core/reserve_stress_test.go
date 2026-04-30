package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Pin every legal and illegal transition in the reservation FSM so the
// allowed set can't drift unnoticed. Each row is a (from, to, allowed?) tuple.
func TestReservationStatus_AllTransitions(t *testing.T) {
	all := []ReservationStatus{
		ReservationStatusActive,
		ReservationStatusSettling,
		ReservationStatusSettled,
		ReservationStatusReleased,
	}

	allowed := map[[2]ReservationStatus]bool{
		{ReservationStatusActive, ReservationStatusSettling}:    true,
		{ReservationStatusActive, ReservationStatusSettled}:     true, // direct settle path
		{ReservationStatusActive, ReservationStatusReleased}:    true,
		{ReservationStatusSettling, ReservationStatusSettled}:   true,
		{ReservationStatusSettling, ReservationStatusReleased}:  true,
	}

	for _, from := range all {
		for _, to := range all {
			pair := [2]ReservationStatus{from, to}
			want := allowed[pair]
			got := from.CanTransitionTo(to)
			assert.Equal(t, want, got, "%s -> %s: want %v, got %v", from, to, want, got)
		}
	}
}

// Terminal statuses (settled, released) must reject every outbound transition.
// Pinning these guards against accidental "resurrect a settled reservation"
// paths that would corrupt accounting.
func TestReservationStatus_TerminalStatesAreSticky(t *testing.T) {
	terminals := []ReservationStatus{ReservationStatusSettled, ReservationStatusReleased}
	all := []ReservationStatus{
		ReservationStatusActive,
		ReservationStatusSettling,
		ReservationStatusSettled,
		ReservationStatusReleased,
	}
	for _, term := range terminals {
		for _, target := range all {
			assert.False(t, term.CanTransitionTo(target), "%s must be terminal, but transition to %s was allowed", term, target)
		}
	}
}

// Self-transitions are not allowed in the reservation FSM. Active -> Active
// would cause double-locks; settled -> settled would imply re-settling.
func TestReservationStatus_NoSelfTransitions(t *testing.T) {
	for _, s := range []ReservationStatus{
		ReservationStatusActive,
		ReservationStatusSettling,
		ReservationStatusSettled,
		ReservationStatusReleased,
	} {
		assert.False(t, s.CanTransitionTo(s), "self-transition %s -> %s must be rejected", s, s)
	}
}

// Bogus statuses must round-trip cleanly: IsValid false, CanTransitionTo false
// from any starting state.
func TestReservationStatus_BogusStatusInert(t *testing.T) {
	bogus := ReservationStatus("frozen")
	assert.False(t, bogus.IsValid())
	assert.False(t, bogus.CanTransitionTo(ReservationStatusActive))
	assert.False(t, ReservationStatusActive.CanTransitionTo(bogus))
}
