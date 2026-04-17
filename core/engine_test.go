package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewEngine_Defaults(t *testing.T) {
	e := NewEngine()
	assert.NotNil(t, e.Logger())
	assert.NotNil(t, e.Metrics())
}

func TestNewEngine_WithOptions(t *testing.T) {
	logger := NopLogger()
	metrics := NopMetrics()
	e := NewEngine(
		WithLogger(logger),
		WithMetrics(metrics),
	)
	assert.Equal(t, logger, e.Logger())
	assert.Equal(t, metrics, e.Metrics())
}
