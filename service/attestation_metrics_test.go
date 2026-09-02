package service_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
	"github.com/azex-ai/ledger/service"
)

// recordingAttestMetrics captures every service.AttestationMetrics call.
type recordingAttestMetrics struct {
	mu             sync.Mutex
	batchResults   []bool
	publishResults []bool
	lags           []int64
}

func (m *recordingAttestMetrics) AttestationBatchResult(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batchResults = append(m.batchResults, ok)
}

func (m *recordingAttestMetrics) AnchorPublishResult(ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.publishResults = append(m.publishResults, ok)
}

func (m *recordingAttestMetrics) AnchorLagSeqs(lag int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lags = append(m.lags, lag)
}

func (m *recordingAttestMetrics) snapshot() ([]bool, []bool, []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]bool(nil), m.batchResults...), append([]bool(nil), m.publishResults...), append([]int64(nil), m.lags...)
}

// alwaysFailingAnchor accepts nothing and knows nothing -- a carrier that is
// down, or a credential that has been revoked.
type alwaysFailingAnchor struct{}

func (alwaysFailingAnchor) Publish(context.Context, int64, []byte) error {
	return errors.New("alwaysFailingAnchor: simulated outage")
}
func (alwaysFailingAnchor) Head(context.Context) (int64, []byte, error) { return 0, nil, nil }

// TestAttestation_AnchorPublishFailureEmitsMetric pins C-M9 / I-M8
// (2026-09-02 audit, tamper-evident.md M-9): a persistently failing anchor
// publish used to produce exactly one signal -- an ERROR log line, which
// under the library's default core.NopLogger went nowhere -- and nothing in
// metrics at all. "This ledger's history has had no external witness for
// three days" was not an alertable fact.
//
// Pinned symbols: service.AttestationMetrics,
// (*service.AttestationService).SetMetrics, RunAttestBatch's publish path.
func TestAttestation_AnchorPublishFailureEmitsMetric(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor := newTestAttestor(t, "attest-metrics-key")
	store := postgres.NewAttestationStore(pool)
	metrics := &recordingAttestMetrics{}
	svc := service.NewAttestationService(store, attestor, nil, alwaysFailingAnchor{}, core.NewEngine())
	svc.SetMetrics(metrics)

	_, seq, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err, "a failing anchor must not block the DB-side chain (design doc §8.3)")
	require.EqualValues(t, 1, seq)

	batches, publishes, _ := metrics.snapshot()
	require.Equal(t, []bool{true}, batches, "the batch itself succeeded and must be reported as such")
	require.Contains(t, publishes, false, "a failed anchor publish must be reported; publishes=%v", publishes)

	// Second run: the chain advances again, the anchor is still down. The
	// failure has to keep being reported -- a signal that fires once and
	// then goes quiet cannot drive an alert on "publishing has been broken
	// for N runs".
	_, _, err = svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	_, publishes, lags := metrics.snapshot()
	failures := 0
	for _, ok := range publishes {
		if !ok {
			failures++
		}
	}
	require.GreaterOrEqual(t, failures, 2, "publishes=%v", publishes)
	require.NotEmpty(t, lags, "anchor lag must be reported on every run, not only when it is non-zero")
	require.Contains(t, lags, int64(1), "with one attestation unpublished the lag is 1; lags=%v", lags)
}

// TestAttestation_HealthyRunReportsZeroLag is the other half: the healthy
// path must emit too. A gauge that only appears when something is wrong is
// indistinguishable from a scrape that failed.
func TestAttestation_HealthyRunReportsZeroLag(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	ctx := context.Background()

	attestor := newTestAttestor(t, "attest-metrics-healthy-key")
	store := postgres.NewAttestationStore(pool)
	anchor := &countingAnchor{}
	metrics := &recordingAttestMetrics{}
	svc := service.NewAttestationService(store, attestor, nil, anchor, core.NewEngine())
	svc.SetMetrics(metrics)

	_, _, err := svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)
	// The second run's catch-up sees the anchor holding seq 1 and the DB at
	// seq 1: caught up, lag 0.
	_, _, err = svc.RunAttestBatch(ctx, 100)
	require.NoError(t, err)

	batches, publishes, lags := metrics.snapshot()
	require.Equal(t, []bool{true, true}, batches)
	require.NotContains(t, publishes, false, "a healthy anchor must not report publish failures")
	require.Contains(t, lags, int64(0), "a caught-up anchor must report lag 0, not silence; lags=%v", lags)
}

// countingAnchor is a minimal in-memory core.Anchor that satisfies the
// no-regression contract.
type countingAnchor struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (a *countingAnchor) Publish(_ context.Context, seq int64, head []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if seq < a.seq {
		return errors.New("countingAnchor: refusing a seq older than the current head")
	}
	a.seq, a.head = seq, append([]byte(nil), head...)
	return nil
}

func (a *countingAnchor) Head(context.Context) (int64, []byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seq, a.head, nil
}
