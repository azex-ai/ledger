package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
)

// captureStdout redirects os.Stdout for the duration of fn and returns
// everything written to it. cmdConfigHistory (like every other cmd*
// function in this package) writes its JSON result via jsonOut, which is
// hardcoded to os.Stdout -- there is no return-value seam to intercept
// instead.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	fn()
	require.NoError(t, w.Close())
	os.Stdout = orig
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

// TestCmdConfigHistory_TableFilter_ReturnsTheRealTriggerWrittenRow exercises
// D-threat's forensic trail end to end through this CLI's own command
// function (not a hand-rolled store bypass, per contract §3 point 6): create
// a currency, deactivate it (an UPDATE), and confirm `config-history --table
// currencies` surfaces the row migration 020's ledger_log_config_table_change
// trigger wrote -- this trail had a writer and grants but, before this
// command existed, no operator-reachable reader (I-N13/I-13).
func TestCmdConfigHistory_TableFilter_ReturnsTheRealTriggerWrittenRow(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: postgrestest.UniqueKey("chx"), Name: "config-history test currency", Exponent: 2,
	})
	require.NoError(t, err)
	require.NoError(t, svc.Currencies().DeactivateCurrency(ctx, cur.UID))

	out := captureStdout(t, func() {
		require.NoError(t, cmdConfigHistory(ctx, svc, []string{"--table", "currencies", "--limit", "50"}))
	})

	var resp struct {
		List []struct {
			TableName string `json:"table_name"`
		} `json:"list"`
		NextCursor string `json:"next_cursor"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &resp))
	require.NotEmpty(t, resp.List, "expected at least the DeactivateCurrency row; got none -- output: %s", out)
	for _, row := range resp.List {
		require.Equal(t, "currencies", row.TableName)
	}
}

// TestCmdConfigHistory_RequiresExactlyOneFilter pins the "three different
// trails, not one filtered three ways" contract: zero or multiple of
// --table/--check/--holder must be rejected before any query runs.
func TestCmdConfigHistory_RequiresExactlyOneFilter(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	err = cmdConfigHistory(ctx, svc, nil)
	require.Error(t, err, "no filter given must be rejected")

	err = cmdConfigHistory(ctx, svc, []string{"--table", "currencies", "--holder", "42"})
	require.Error(t, err, "two filters given must be rejected")
}

// TestParseHistoryBound covers the day-suffix shorthand docs/RUNBOOK.md
// documents (`--since 30d`) alongside the RFC3339 fallback.
func TestParseHistoryBound(t *testing.T) {
	empty, err := parseHistoryBound("")
	require.NoError(t, err)
	require.True(t, empty.IsZero())

	days, err := parseHistoryBound("7d")
	require.NoError(t, err)
	require.False(t, days.IsZero())

	ts, err := parseHistoryBound("2026-01-01T00:00:00Z")
	require.NoError(t, err)
	require.Equal(t, 2026, ts.Year())

	_, err = parseHistoryBound("not-a-bound")
	require.Error(t, err)
}
