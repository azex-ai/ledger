package core

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"transient", ErrTransient, true},
		{"wrapped transient", fmt.Errorf("postgres: post journal: %w: %w", fmt.Errorf("serialization failure"), ErrTransient), true},
		{"attestor unavailable", ErrAttestorUnavailable, true},
		{"rollup pending", ErrRollupPending, true},
		{"not found", ErrNotFound, false},
		{"invalid input", ErrInvalidInput, false},
		{"insufficient balance", ErrInsufficientBalance, false},
		{"duplicate journal", ErrDuplicateJournal, false},
		{"unbalanced journal", ErrUnbalancedJournal, false},
		{"invalid transition", ErrInvalidTransition, false},
		{"conflict", ErrConflict, false},
		{"precision exceeded", ErrPrecisionExceeded, false},
		{"account frozen", ErrAccountFrozen, false},
		{"account closed", ErrAccountClosed, false},
		{"period closed", ErrPeriodClosed, false},
		{"unauthorized journal", ErrUnauthorizedJournal, false},
		{"wrapped unauthorized journal", fmt.Errorf("core: verify journal auth: journal has no stored digest: %w", ErrUnauthorizedJournal), false},
		{"unclassified error defaults retryable", fmt.Errorf("connection reset by peer"), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsRetryable(tc.err))
		})
	}
}
