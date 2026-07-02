package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCleanupContext_SurvivesParentCancellation verifies the returned context
// is not cancelled when the parent is, but still carries parent values and
// eventually expires on its own bounded timeout.
func TestCleanupContext_SurvivesParentCancellation(t *testing.T) {
	type ctxKey string
	const key ctxKey = "trace_id"

	parent, cancel := context.WithCancel(context.Background())
	parent = context.WithValue(parent, key, "abc123")

	cleanupCtx, cleanupCancel := cleanupContext(parent)
	defer cleanupCancel()

	cancel() // parent cancelled — simulates a shutdown signal firing

	assert.NoError(t, cleanupCtx.Err(), "cleanup ctx must not be cancelled when the parent is")
	assert.Equal(t, "abc123", cleanupCtx.Value(key), "cleanup ctx must still carry parent values")

	deadline, ok := cleanupCtx.Deadline()
	require.True(t, ok, "cleanup ctx must carry its own bounded deadline")
	assert.False(t, deadline.IsZero())
}
