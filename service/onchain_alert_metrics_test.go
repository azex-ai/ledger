package service

// M-10 (W3 adversarial review of the gates, 2026-09-03): behaviour pins for
// the deposit-path alert metrics.
//
// observability/emission_coverage_test.go asserts every core.Metrics method
// has a production call site; it used to do so by scanning source text, so a
// comment satisfied it (that half is now AST-based). The reviewer's other
// finding was a census: four methods -- DepositReorgDetected,
// DepositReviewRequired, RegistrationRescanFailed, SweepUnattributed -- were
// referenced by no test anywhere. For those four, deleting the production
// call site left the coverage gate as the ONLY thing that could notice, and
// that gate answers "some source mentions it", never "the code path emits
// it".
//
// These are the three that are reachable without a chain fixture. Each drives
// the real production function with stub ports and asserts the emission, so
// deleting the emit line goes red here. SweepUnattributed's emit sits inside
// sweepTick, past a Scanner/Sweeper/gas-price/address-batch fixture; it is
// registered in emission_coverage_test.go's untestedAlertMetrics with that
// reason rather than left silently uncovered.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// alertMetricsRecorder records the deposit-path alert emissions. It embeds
// core.Metrics so the (many) methods these paths do not touch panic loudly
// rather than being silently satisfied -- an embedded nil interface means
// "this test claimed a path emits nothing else, and will say so if it does".
type alertMetricsRecorder struct {
	core.Metrics
	mu             sync.Mutex
	reorgDetected  []int64
	reviewRequired []reviewRequiredCall
	rescanFailed   []int64
}

type reviewRequiredCall struct {
	chainID int64
	reason  string
}

func (m *alertMetricsRecorder) DepositReorgDetected(chainID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reorgDetected = append(m.reorgDetected, chainID)
}

func (m *alertMetricsRecorder) DepositReviewRequired(chainID int64, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reviewRequired = append(m.reviewRequired, reviewRequiredCall{chainID, reason})
}

func (m *alertMetricsRecorder) RegistrationRescanFailed(chainID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rescanFailed = append(m.rescanFailed, chainID)
}

func (m *alertMetricsRecorder) snapshot() ([]int64, []reviewRequiredCall, []int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int64(nil), m.reorgDetected...),
		append([]reviewRequiredCall(nil), m.reviewRequired...),
		append([]int64(nil), m.rescanFailed...)
}

// TestHandleReorg_EmitsDepositReorgDetected pins the deep-reorg alert. Under
// the default (manual) policy the durable anomaly row and this metric are the
// entire handling: a previously-confirmed deposit whose transaction left the
// canonical chain is credited balance with no chain event behind it, and
// nothing else in the system will raise its hand.
func TestHandleReorg_EmitsDepositReorgDetected(t *testing.T) {
	metrics := &alertMetricsRecorder{}
	// No ReorgRecorder: recordReorgAnomaly logs that fact and returns, which
	// keeps this test on the emission path without a store fixture. The
	// anomaly row itself is pinned by the reorg store's own tests.
	o := &Onchain{deps: OnchainDeps{Logger: core.NopLogger(), Metrics: metrics}}

	// misses=1: the FIRST observation. Reporting is unconditional -- only
	// auto_reverse's debit waits for corroboration (M-1) -- so one
	// observation must already produce the counter.
	o.handleReorg(context.Background(), &core.Booking{UID: "bk-reorg", ChannelRef: "0xdead"}, 8453, 1)

	reorgs, _, _ := metrics.snapshot()
	assert.Equal(t, []int64{8453}, reorgs,
		"a deep reorg on a confirmed deposit must emit DepositReorgDetected(chainID) -- under the default manual policy the alert IS the handling")
}

// stubReviewBooker accepts the transition to review and hands back the
// booking as reloaded.
type stubReviewBooker struct {
	transitions []core.TransitionInput
}

func (s *stubReviewBooker) CreateBooking(context.Context, core.CreateBookingInput) (*core.Booking, error) {
	return nil, errors.New("stubReviewBooker: CreateBooking not used by this path")
}

func (s *stubReviewBooker) Transition(_ context.Context, in core.TransitionInput) (*core.Event, error) {
	s.transitions = append(s.transitions, in)
	return &core.Event{UID: "ev-review"}, nil
}

type stubBookingReader struct{ booking core.Booking }

func (s stubBookingReader) GetBooking(context.Context, string) (*core.Booking, error) {
	b := s.booking
	return &b, nil
}

func (s stubBookingReader) ListBookings(context.Context, core.BookingFilter) ([]core.Booking, string, error) {
	return nil, "", errors.New("stubBookingReader: ListBookings not used by this path")
}

// TestRouteToReview_EmitsDepositReviewRequired pins the review-queue alert. A
// deposit parked for human review credits nothing until someone acts on it
// (I-21), so an unnoticed review queue is a user whose deposit silently never
// arrives.
func TestRouteToReview_EmitsDepositReviewRequired(t *testing.T) {
	metrics := &alertMetricsRecorder{}
	booker := &stubReviewBooker{}
	booking := core.Booking{
		UID: "bk-review",
		// parseDepositMeta requires the whole deposit-identity set; a
		// partial map reads as "not a chain deposit" and yields chain 0.
		Metadata: map[string]string{
			"chain_id":     "10",
			"tx_hash":      "0xbeef",
			"txlog_seq":    "0",
			"block_number": "42",
		},
	}
	o := &Onchain{deps: OnchainDeps{
		Logger:        core.NopLogger(),
		Metrics:       metrics,
		Booker:        booker,
		BookingReader: stubBookingReader{booking: booking},
	}}

	_, err := o.routeToReview(context.Background(), &booking, "0xbeef", "over_ceiling")
	require.NoError(t, err)

	_, reviews, _ := metrics.snapshot()
	require.Len(t, reviews, 1, "routing a deposit to review must emit DepositReviewRequired")
	assert.Equal(t, int64(10), reviews[0].chainID, "the chain id comes from the booking's deposit metadata")
	assert.Equal(t, "over_ceiling", reviews[0].reason, "the reason is what tells on-call which gate parked the deposit")
}

// stubRescanStore hands out one claimed job and swallows the retry write.
type stubRescanStore struct {
	job     core.RegistrationRescan
	retried []string
}

func (s *stubRescanStore) EnqueueRegistrationRescans(context.Context, []core.RegistrationRescan) error {
	return nil
}

func (s *stubRescanStore) ClaimRegistrationRescans(context.Context, int, time.Duration) ([]core.RegistrationRescan, error) {
	return []core.RegistrationRescan{s.job}, nil
}

func (s *stubRescanStore) AdvanceRegistrationRescan(context.Context, string, int64, bool, int32) error {
	return errors.New("stubRescanStore: Advance must not be reached when the scan fails")
}

func (s *stubRescanStore) RetryRegistrationRescan(_ context.Context, uid, _ string, _ time.Time, _ int32) error {
	s.retried = append(s.retried, uid)
	return nil
}

// failingChainReader is an RPC endpoint that is down.
type failingChainReader struct{}

func (failingChainReader) LatestBlock(context.Context, int64) (int64, error) {
	return 0, errors.New("failingChainReader: simulated RPC outage")
}

func (failingChainReader) FetchDeposits(context.Context, int64, int64, int64, []string) ([]core.DepositSighting, error) {
	return nil, errors.New("failingChainReader: simulated RPC outage")
}

func (failingChainReader) TxIncluded(context.Context, int64, string) (bool, error) {
	return false, errors.New("failingChainReader: simulated RPC outage")
}

// TestRunRegistrationRescansOnce_EmitsRegistrationRescanFailed pins the
// rescan-failure alert. A registration rescan is what catches a deposit sent
// to an address between its derivation and its registration; while it keeps
// failing, those deposits are invisible to the ledger, and the retry is
// persisted rather than surfaced -- so this metric is the only thing that
// distinguishes "retrying" from "wedged".
func TestRunRegistrationRescansOnce_EmitsRegistrationRescanFailed(t *testing.T) {
	metrics := &alertMetricsRecorder{}
	store := &stubRescanStore{job: core.RegistrationRescan{
		UID: "rs-1", ChainID: 137, Address: "0xabc", NextBlock: 100, Attempts: 1,
	}}
	o := &Onchain{
		deps: OnchainDeps{
			Logger:              core.NopLogger(),
			Metrics:             metrics,
			Reader:              failingChainReader{},
			RegistrationRescans: store,
		},
		maxConcurrentRescans:      1,
		registrationRescanTimeout: time.Second,
	}

	// The claim itself succeeds and one job fails, so the tick reports that
	// failure -- which is the point of M-9's counted ticks.
	require.Error(t, o.runRegistrationRescansOnce(context.Background()))

	_, _, failures := metrics.snapshot()
	assert.Equal(t, []int64{137}, failures,
		"a registration rescan that failed its whole window must emit RegistrationRescanFailed(chainID): the retry is persisted silently, so this is the only signal that it is not progressing")
	assert.Equal(t, []string{"rs-1"}, store.retried, "sanity: the failure path is the one that persists a retry")
}

// stubCursorStore is a chain cursor that has never been written.
type stubCursorStore struct{ set []int64 }

func (s *stubCursorStore) GetCursor(context.Context, int64) (*core.ChainCursor, error) {
	return nil, core.ErrNotFound
}

func (s *stubCursorStore) SetCursor(_ context.Context, _ int64, lastScanned int64) error {
	s.set = append(s.set, lastScanned)
	return nil
}

// emptyRegistry has no registered deposit addresses, which is the watcher's
// "nothing to look for, still advance the cursor" path.
type emptyRegistry struct{}

func (emptyRegistry) EnsureAddress(context.Context, core.AddressRegistrationInput) (*core.DepositAddress, error) {
	return nil, errors.New("emptyRegistry: EnsureAddress not used by this path")
}

func (emptyRegistry) GetByAddress(context.Context, string) (*core.DepositAddress, error) {
	return nil, core.ErrNotFound
}

func (emptyRegistry) ListAddresses(context.Context) ([]core.DepositAddress, error) {
	return nil, nil
}

// tipChainReader is a healthy RPC endpoint reporting a fixed head.
type tipChainReader struct{ tip int64 }

func (r tipChainReader) LatestBlock(context.Context, int64) (int64, error) { return r.tip, nil }

func (tipChainReader) FetchDeposits(context.Context, int64, int64, int64, []string) ([]core.DepositSighting, error) {
	return nil, nil
}

func (tipChainReader) TxIncluded(context.Context, int64, string) (bool, error) { return true, nil }

// TestScanChainOnce_EmitsChainCursorLag pins the watcher's lag gauge. It is
// the only continuous signal that deposits are being seen at all: a cursor
// held still by a blocking ingest failure (I-52 deliberately refuses to
// advance past one) looks identical to an idle chain in every other output,
// and the difference is deposits nobody will ever scan again.
func TestScanChainOnce_EmitsChainCursorLag(t *testing.T) {
	metrics := &lagMetricsRecorder{}
	cursors := &stubCursorStore{}
	o := &Onchain{
		deps: OnchainDeps{
			Logger:   core.NopLogger(),
			Metrics:  metrics,
			Cursors:  cursors,
			Reader:   tipChainReader{tip: 1000},
			Registry: emptyRegistry{},
		},
		chains:           core.ChainSet{7: {ChainID: 7, Confirmations: 12}},
		maxBlocksPerScan: 500,
	}

	require.NoError(t, o.scanChainOnce(context.Background(), 7))

	require.Len(t, cursors.set, 1, "sanity: with no registered addresses the watcher still advances the cursor")
	lags := metrics.snapshotLags()
	require.Len(t, lags, 1, "every watcher tick must report ChainCursorLag -- a silent watcher and a stalled one look the same otherwise")
	assert.Equal(t, int64(7), lags[0].chainID)
	assert.Equal(t, 1000-cursors.set[0], lags[0].lagBlocks,
		"the lag is measured against the chain head, not against the safe tip: a cursor that stops advancing must show a GROWING number")

	// M-8: the lag above can only be computed once LatestBlock has answered,
	// so it is not a liveness signal. Every tick also reports how long the
	// cursor has gone without moving, which is.
	ages := metrics.snapshotAges()
	require.Len(t, ages, 1, "every watcher tick must also report ChainCursorAdvanceAge")
	assert.Equal(t, int64(7), ages[0].chainID)
}

// lagMetricsRecorder records the watcher's two cursor gauges.
//
// Embeds core.NoopMetrics (the value, not the core.Metrics interface): an
// embedded nil interface panics on the first method this recorder does not
// override, which is how adding ChainCursorAdvanceAge turned this pin into
// a segfault rather than a failure. The embedding pattern core.Metrics'
// own doc comment recommends does not have that failure mode.
type lagMetricsRecorder struct {
	core.NoopMetrics
	mu   sync.Mutex
	lags []lagCall
	ages []ageCall
}

type ageCall struct {
	chainID int64
	age     time.Duration
}

func (m *lagMetricsRecorder) ChainCursorAdvanceAge(chainID int64, age time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ages = append(m.ages, ageCall{chainID, age})
}

func (m *lagMetricsRecorder) snapshotAges() []ageCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]ageCall(nil), m.ages...)
}

type lagCall struct {
	chainID   int64
	lagBlocks int64
}

func (m *lagMetricsRecorder) ChainCursorLag(chainID, lagBlocks int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lags = append(m.lags, lagCall{chainID, lagBlocks})
}

func (m *lagMetricsRecorder) snapshotLags() []lagCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]lagCall(nil), m.lags...)
}
