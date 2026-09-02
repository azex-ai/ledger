package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/azex-ai/ledger/core"
)

// unreachableAttestationStore panics if any method is called -- this test
// only exercises Worker.Run's startup logging, never the periodic
// attestation job itself (AttestInterval defaults to 60s, far longer than
// this test's context timeout, so RunAttestBatch never ticks).
type unreachableAttestationStore struct{}

func (unreachableAttestationStore) LatestAttestation(context.Context) (core.Attestation, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) UncoveredEntries(context.Context, int32) ([]core.AttestedEntry, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) InsertAttestation(context.Context, core.Attestation, []int64, [][]byte, []core.JournalAuthVerdict) (core.Attestation, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) EntriesForAttestation(context.Context, int64) ([]core.AttestedEntry, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) LeafHashesForAttestation(context.Context, int64) ([]core.AttestedLeaf, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) ListAttestationsFrom(context.Context, int64, int32) ([]core.Attestation, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) JournalAuthMaterial(context.Context, []int64) (map[int64]core.JournalAuthMaterial, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) RecordAnchorObservation(context.Context, int64, []byte) error {
	panic("unreachableAttestationStore: not expected to be called in this test")
}
func (unreachableAttestationStore) HighestObservedAnchorSeq(context.Context) (int64, error) {
	panic("unreachableAttestationStore: not expected to be called in this test")
}

type stubAttestor struct{}

func (stubAttestor) Sign(context.Context, []byte) ([]byte, string, error) {
	return nil, "", errors.New("stubAttestor: not expected to be called in this test")
}

type stubAnchor struct{}

func (stubAnchor) Publish(context.Context, int64, []byte) error {
	return errors.New("stubAnchor: not expected to be called in this test")
}
func (stubAnchor) Head(context.Context) (int64, []byte, error) {
	return 0, nil, errors.New("stubAnchor: not expected to be called in this test")
}

// recordingLevelLogger captures every log call's level, message, and args --
// worker_test.go's recordingLogger (service/expiration_test.go) drops Warn
// calls entirely (`func (l *recordingLogger) Warn(string, ...any) {}`),
// which cannot pin this test's Warn assertion, hence a separate type here
// rather than widening a shared mock other tests do not need changed.
// Worker.Run starts several concurrent job goroutines (rollup, expiration,
// reconcile, snapshot, system_rollup, attestation), each logging via
// runLoop's own "worker: started" Info call independently -- this mock is
// genuinely shared across goroutines, so it needs its own lock (a real
// core.Logger implementation, e.g. slog's, is concurrency-safe; a test
// double standing in for one must be too).
type recordingLevelLogger struct {
	mu    sync.Mutex
	infos []logCall
	warns []logCall
}

type logCall struct {
	msg  string
	args []any
}

func (l *recordingLevelLogger) Info(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.infos = append(l.infos, logCall{msg, args})
}
func (l *recordingLevelLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, logCall{msg, args})
}
func (l *recordingLevelLogger) Error(msg string, args ...any) {}

// argValue returns the value paired with key in a slog-style args list
// ("key", value, "key", value, ...), or nil if key is absent.
func argValue(args []any, key string) any {
	for i := 0; i+1 < len(args); i += 2 {
		if k, ok := args[i].(string); ok && k == key {
			return args[i+1]
		}
	}
	return nil
}

func newMinimalWorkerForAttestationLogTest(t *testing.T, logger core.Logger) *Worker {
	t.Helper()
	engine := core.NewEngine(core.WithLogger(logger))

	rollupSvc := NewRollupService(&mockRollupQueuer{}, newMockCheckpointRW(), &mockEntrySummer{}, &mockClassificationLister{}, engine)
	expirationSvc := NewExpirationService(&mockExpiredReservationFinder{}, &mockReservationReleaser{}, nil, nil, nil, engine)
	reconcileSvc := NewReconciliationService(
		&mockGlobalSummer{totals: []CurrencyReconcileTotals{{CurrencyID: 1, Debit: decimal.Zero, Credit: decimal.Zero}}},
		&mockAccountEntrySummer{}, &mockCheckpointReader{}, &mockClassificationLister{}, engine,
	)
	snapshotSvc := NewSnapshotService(&mockHistoricalBalanceLister{}, &mockSnapshotWriter{}, engine)
	systemRollupSvc := NewSystemRollupService(&mockCheckpointAggregator{}, &mockSystemRollupWriter{}, engine)

	config := WorkerConfig{
		RollupInterval:       time.Minute,
		RollupBatchSize:      10,
		ExpirationInterval:   time.Minute,
		ExpirationBatchSize:  10,
		ReconcileInterval:    time.Minute,
		SnapshotInterval:     time.Minute,
		SystemRollupInterval: time.Minute,
		// AttestInterval must be > 0: NewWorker does not merge in
		// DefaultWorkerConfig (that is Service.Worker's job), and runLoop's
		// "interval <= 0" branch itself logs a Warn ("worker: skipping
		// job") -- a zero value here would pollute logger.warns with an
		// unrelated Warn in every subtest, defeating the anchored-case
		// assertion that no Warn was logged. A full minute is far longer
		// than this test's context timeout, so the ticker never fires and
		// RunAttestBatch (which unreachableAttestationStore/stubAttestor/
		// stubAnchor above would panic or error on) is never reached.
		AttestInterval: time.Minute,
	}

	return NewWorker(rollupSvc, expirationSvc, reconcileSvc, snapshotSvc, systemRollupSvc, config, engine)
}

// TestWorker_Run_LogsAttestationAnchorState pins m-7
// (`.local/independent-review-2026-08-26.md`): the startup log's
// "attestation" field only ever reported whether the P6 batch job is
// running at all, never whether an anchor was wired in -- a deployment
// running SetAttestor with a nil anchor (Service.Worker's own auto-wiring
// default, ledger.go) looked identical in the log to one with a real
// anchor, even though only the anchored case lets VerifyLedger detect a
// wholesale history rewrite (service/attestation.go's anchor field
// comment). This asserts both halves of the fix: the new
// "attestation_anchor" field reports the true state, and an anchorless
// attestation job additionally logs a Warn.
func TestWorker_Run_LogsAttestationAnchorState(t *testing.T) {
	t.Run("attestation enabled, anchor nil -- must warn and report attestation_anchor=false", func(t *testing.T) {
		logger := &recordingLevelLogger{}
		w := newMinimalWorkerForAttestationLogTest(t, logger)
		w.SetAttestor(NewAttestationService(unreachableAttestationStore{}, stubAttestor{}, nil, nil, core.NewEngine(core.WithLogger(logger))))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		assert.NoError(t, w.Run(ctx))

		startCall := findLogCall(logger.infos, "worker: starting")
		if assert.NotNil(t, startCall, "expected a 'worker: starting' Info log") {
			assert.Equal(t, true, argValue(startCall.args, "attestation"))
			assert.Equal(t, false, argValue(startCall.args, "attestation_anchor"),
				"anchorless attestation must not be indistinguishable from a real anchor in the startup log")
		}

		assert.NotNil(t, findWarnContaining(logger.warns, "no anchor configured"),
			"an anchorless attestation job must log a Warn at startup, not just an Info flag a reader could miss")
	})

	t.Run("attestation enabled with anchor -- no warn, attestation_anchor=true", func(t *testing.T) {
		logger := &recordingLevelLogger{}
		w := newMinimalWorkerForAttestationLogTest(t, logger)
		w.SetAttestor(NewAttestationService(unreachableAttestationStore{}, stubAttestor{}, nil, stubAnchor{}, core.NewEngine(core.WithLogger(logger))))

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		assert.NoError(t, w.Run(ctx))

		startCall := findLogCall(logger.infos, "worker: starting")
		if assert.NotNil(t, startCall) {
			assert.Equal(t, true, argValue(startCall.args, "attestation"))
			assert.Equal(t, true, argValue(startCall.args, "attestation_anchor"))
		}
		assert.Nil(t, findWarnContaining(logger.warns, "no anchor configured"),
			"a properly anchored attestation job must not warn about the anchor at startup")
	})

	t.Run("attestation disabled -- no warn, attestation_anchor=false", func(t *testing.T) {
		logger := &recordingLevelLogger{}
		w := newMinimalWorkerForAttestationLogTest(t, logger)
		// No SetAttestor call.

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		assert.NoError(t, w.Run(ctx))

		startCall := findLogCall(logger.infos, "worker: starting")
		if assert.NotNil(t, startCall) {
			assert.Equal(t, false, argValue(startCall.args, "attestation"))
			assert.Equal(t, false, argValue(startCall.args, "attestation_anchor"))
		}
		assert.Nil(t, findWarnContaining(logger.warns, "no anchor configured"),
			"attestation disabled entirely is not the degraded state this warns about")
	})
}

// findWarnContaining locates a Warn whose message contains sub. Worker.Run
// emits one Warn per degraded-but-permitted state it is in (see
// StartupReport.Warnings), so these subtests match the anchor warning by
// content rather than asserting the whole slice is empty -- a
// pool-less test Worker legitimately also warns about leader election.
func findWarnContaining(calls []logCall, sub string) *logCall {
	for i := range calls {
		if strings.Contains(calls[i].msg, sub) {
			return &calls[i]
		}
	}
	return nil
}

func findLogCall(calls []logCall, msg string) *logCall {
	for i := range calls {
		if calls[i].msg == msg {
			return &calls[i]
		}
	}
	return nil
}
