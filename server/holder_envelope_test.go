package server_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/server"
)

// TestHolderListEnvelopes_CarryNullNextCursor is H-m4's pin.
//
// GET /holder/balances and GET /holder/holds used to answer
// `httpx.OK(w, map[string]any{"list": out})`: no next_cursor key at all,
// while the other twelve list envelopes in this API answer
// `"next_cursor": null`. api-contract.md §6 wants one comparable sentinel;
// two spellings mean a consumer's generic paging helper reads `undefined`
// on these two routes and `null` everywhere else. The map form also left
// the envelope layer with no Go struct for the openapi contract gate to
// reflect on -- it could only ever check the item shape.
func TestHolderListEnvelopes_CarryNullNextCursor(t *testing.T) {
	now := time.Now()
	secret := []byte(testHolderSecret)
	stub := &stubHolderReader{}
	ts := mountedHolderAPI(t, stub, server.HolderConfig{TokenSecret: secret})
	token, err := server.MintHolderToken(secret, 42, time.Hour, now)
	require.NoError(t, err)

	for _, path := range []string{"/api/v1/holder/balances", "/api/v1/holder/holds"} {
		t.Run(path, func(t *testing.T) {
			resp, body := get(t, ts, path, token)
			require.Equal(t, http.StatusOK, resp.StatusCode)

			data, ok := body["data"].(map[string]any)
			require.True(t, ok, "data must be an object")
			require.Contains(t, data, "list")
			require.Contains(t, data, "next_cursor",
				"every list envelope in this API carries a next_cursor key (api-contract.md §6); this route omitted it entirely")
			assert.Nil(t, data["next_cursor"],
				"this route returns its full result set, so the sentinel is literal null -- never an omitted key or \"\"")
		})
	}
}
