package server

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_ProductionRequiresAPIKeys(t *testing.T) {
	t.Setenv("ENV", "production")
	t.Setenv("CORS_ALLOWED_ORIGIN", "https://ledger.example")
	t.Setenv("API_KEYS", "")

	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "API_KEYS")
}

func TestLoadConfig_DevAllowsNoAPIKeys(t *testing.T) {
	t.Setenv("ENV", "dev")
	t.Setenv("CORS_ALLOWED_ORIGIN", "")
	t.Setenv("API_KEYS", "")

	cfg, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, cfg.APIKeys)
}
