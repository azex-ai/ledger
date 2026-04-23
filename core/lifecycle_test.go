package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validLifecycle() *Lifecycle {
	return &Lifecycle{
		Initial:  "pending",
		Terminal: []Status{"done", "failed"},
		Transitions: map[Status][]Status{
			"pending":    {"active", "failed"},
			"active":     {"done", "failed"},
		},
	}
}

func TestLifecycle_Validate(t *testing.T) {
	tests := []struct {
		name      string
		lifecycle *Lifecycle
		wantErr   bool
		errMsg    string
	}{
		{
			name:      "valid lifecycle",
			lifecycle: validLifecycle(),
		},
		{
			name: "empty initial",
			lifecycle: &Lifecycle{
				Initial:  "",
				Terminal: []Status{"done"},
				Transitions: map[Status][]Status{
					"pending": {"done"},
				},
			},
			wantErr: true,
			errMsg:  "initial status must not be empty",
		},
		{
			name: "initial has no outgoing transitions",
			lifecycle: &Lifecycle{
				Initial:  "pending",
				Terminal: []Status{"done"},
				Transitions: map[Status][]Status{
					"active": {"done"},
				},
			},
			wantErr: true,
			errMsg:  "must have outgoing transitions",
		},
		{
			name: "terminal state has outgoing transitions",
			lifecycle: &Lifecycle{
				Initial:  "pending",
				Terminal: []Status{"done"},
				Transitions: map[Status][]Status{
					"pending": {"done"},
					"done":    {"pending"},
				},
			},
			wantErr: true,
			errMsg:  "must not have outgoing transitions",
		},
		{
			name: "transition target not defined anywhere",
			lifecycle: &Lifecycle{
				Initial:  "pending",
				Terminal: []Status{"done"},
				Transitions: map[Status][]Status{
					"pending": {"unknown"},
				},
			},
			wantErr: true,
			errMsg:  "targets undefined status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.lifecycle.Validate()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errMsg)
				assert.ErrorIs(t, err, ErrInvalidInput)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestLifecycle_CanTransition(t *testing.T) {
	lc := validLifecycle()

	tests := []struct {
		name string
		from Status
		to   Status
		want bool
	}{
		{"valid transition", "pending", "active", true},
		{"invalid transition", "pending", "done", false},
		{"from terminal state", "done", "pending", false},
		{"from unknown state", "nonexistent", "active", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.CanTransition(tt.from, tt.to))
		})
	}
}

func TestLifecycle_IsTerminal(t *testing.T) {
	lc := validLifecycle()

	tests := []struct {
		name   string
		status Status
		want   bool
	}{
		{"terminal state", "done", true},
		{"another terminal state", "failed", true},
		{"non-terminal state", "pending", false},
		{"unknown state", "nonexistent", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lc.IsTerminal(tt.status))
		})
	}
}
