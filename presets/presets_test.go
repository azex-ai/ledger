package presets

import (
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/stretchr/testify/assert"
)

func TestDepositLifecycle_Validate(t *testing.T) {
	assert.NoError(t, DepositLifecycle.Validate())
}

func TestDepositLifecycle_Transitions(t *testing.T) {
	lc := DepositLifecycle

	tests := []struct {
		name string
		from core.Status
		to   core.Status
		want bool
	}{
		{"pending -> confirming", "pending", "confirming", true},
		{"pending -> confirmed (must go through confirming)", "pending", "confirmed", false},
		{"pending -> failed", "pending", "failed", true},
		{"pending -> expired", "pending", "expired", true},
		{"confirming -> confirmed", "confirming", "confirmed", true},
		{"confirming -> failed", "confirming", "failed", true},
		{"confirmed -> anything (terminal)", "confirmed", "pending", false},
		{"failed -> anything (terminal)", "failed", "pending", false},
		{"expired -> anything (terminal)", "expired", "pending", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}

func TestWithdrawalLifecycle_Validate(t *testing.T) {
	assert.NoError(t, WithdrawalLifecycle.Validate())
}

func TestWithdrawalLifecycle_Transitions(t *testing.T) {
	lc := WithdrawalLifecycle

	tests := []struct {
		name string
		from core.Status
		to   core.Status
		want bool
	}{
		{"locked -> reserved", "locked", "reserved", true},
		{"reserved -> reviewing", "reserved", "reviewing", true},
		{"reserved -> processing", "reserved", "processing", true},
		{"reviewing -> processing", "reviewing", "processing", true},
		{"reviewing -> failed", "reviewing", "failed", true},
		{"processing -> confirmed", "processing", "confirmed", true},
		{"processing -> failed", "processing", "failed", true},
		{"processing -> expired", "processing", "expired", true},
		{"failed -> reserved (retry)", "failed", "reserved", true},
		{"expired -> anything (terminal)", "expired", "reserved", false},
		{"confirmed -> anything (terminal)", "confirmed", "locked", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}
