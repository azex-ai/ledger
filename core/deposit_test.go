package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDepositStatus_Transitions(t *testing.T) {
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusConfirming))
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusFailed))
	assert.True(t, DepositStatusPending.CanTransitionTo(DepositStatusExpired))
	assert.True(t, DepositStatusConfirming.CanTransitionTo(DepositStatusConfirmed))
	assert.True(t, DepositStatusConfirming.CanTransitionTo(DepositStatusFailed))
	assert.False(t, DepositStatusConfirmed.CanTransitionTo(DepositStatusPending))
	assert.False(t, DepositStatusFailed.CanTransitionTo(DepositStatusConfirmed))
}
