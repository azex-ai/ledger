package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestOnchain_RunLoop_PanicDoesNotCrashProcess pins I-M9 / C-M7 part 3
// (d-tamper handoff): none of Onchain's five jobs (registration_rescan,
// watch, recheck, reorg_recheck, sweep) route through service.Worker's
// runLoop -- Onchain keeps its own copy -- so a panic inside a single tick
// (a ChainReader/ChainScanner/Sweeper implementation bug, for example) used
// to propagate straight through errgroup.Wait and out of Run, taking down
// whatever process called `go onchain... Run(ctx)` with it. This test is
// itself the regression signal: without runLoop's recover, the panic
// reaches testing's own handler and the test process dies instead of
// failing cleanly.
func TestOnchain_RunLoop_PanicDoesNotCrashProcess(t *testing.T) {
	metrics := newJobMetricsRecorder()
	o := &Onchain{deps: OnchainDeps{Logger: core.NopLogger(), Metrics: metrics}}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()

	err := o.runLoop(ctx, "test_job", 10*time.Millisecond, func(context.Context) {
		panic("simulated job bug")
	})
	require.NoError(t, err, "runLoop must return cleanly (via ctx cancellation), not propagate the tick's panic")

	assert.Greater(t, metrics.count(metrics.panicked, "test_job"), 0, "JobPanicked(\"test_job\") must be emitted at least once")
}
