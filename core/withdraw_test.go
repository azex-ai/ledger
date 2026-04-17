package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestWithdrawStatus_Transitions(t *testing.T) {
	// Happy path
	assert.True(t, WithdrawStatusLocked.CanTransitionTo(WithdrawStatusReserved))
	assert.True(t, WithdrawStatusReserved.CanTransitionTo(WithdrawStatusReviewing))
	assert.True(t, WithdrawStatusReserved.CanTransitionTo(WithdrawStatusProcessing)) // skip review
	assert.True(t, WithdrawStatusReviewing.CanTransitionTo(WithdrawStatusProcessing))
	assert.True(t, WithdrawStatusProcessing.CanTransitionTo(WithdrawStatusConfirmed))
	// Failure + retry
	assert.True(t, WithdrawStatusProcessing.CanTransitionTo(WithdrawStatusFailed))
	assert.True(t, WithdrawStatusFailed.CanTransitionTo(WithdrawStatusReserved)) // retry
	// Expired is terminal
	assert.False(t, WithdrawStatusExpired.CanTransitionTo(WithdrawStatusReserved))
}
