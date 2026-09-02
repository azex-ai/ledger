package ledger_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// This file pins what (*Service).EnableOnchain actually WIRES, from the two
// calls a consumer makes -- ledger.New(pool) then svc.EnableOnchain(...) --
// rather than from a hand-assembled service.OnchainDeps.
//
// The distinction is the whole point. The 2026-09-02 audit's most-repeated
// finding on this facade is a mechanism that exists in service/ and is never
// connected here, with nothing going red: F-M1 deleted ledger.go's
// SetPartitionService / SetPool lines and `go test ./...` stayed green, and
// I-R1 found EventStore.SetLogger with a setter, 22 lines of doc, and zero
// call sites. Both wiring lines below would fail exactly that way if they
// were removed, so each has a pin whose subject IS the line.

// wiringChainReader is a core.ChainReader that only counts calls -- enough
// to tell "the watcher ran this tick" from "it did not", which is what the
// advisory-lock pin below turns on.
type wiringChainReader struct {
	mu               sync.Mutex
	latestBlockCalls int
	fetchCalls       int
}

func (r *wiringChainReader) LatestBlock(ctx context.Context, chainID int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latestBlockCalls++
	return 1_000, nil
}

func (r *wiringChainReader) FetchDeposits(ctx context.Context, chainID, fromBlock, toBlock int64, addresses []string) ([]core.DepositSighting, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetchCalls++
	return nil, nil
}

func (r *wiringChainReader) TxIncluded(ctx context.Context, chainID int64, txHash string) (bool, error) {
	return true, nil
}

func (r *wiringChainReader) calls() (latestBlock, fetch int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.latestBlockCalls, r.fetchCalls
}

// wiringChainSet is a fully configured single-chain ChainSet: both
// secure-by-default startup gates (AutoCreditCeiling, ReconcileFailureLimit)
// are deliberately satisfied so that any error Run returns below is about
// the wiring under test and nothing else.
func wiringChainSet() core.ChainSet {
	return core.ChainSet{
		1: {
			ChainID:       1,
			Confirmations: 6,
			Factory:       "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323",
			InitHash:      "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ec",
			CreditTokens: map[string]core.TokenConfig{
				"0xusdt": {
					TokenAddress:          "0xusdt",
					CurrencyCode:          "USDT",
					Decimals:              6,
					AutoCreditCeiling:     decimal.NewFromInt(1_000),
					ReconcileFailureLimit: 3,
				},
			},
		},
	}
}

// TestServiceEnableOnchain_WiresTheReorgRecorder pins the facade half of
// G-M8. service.Onchain.Run refuses to start when a ChainReader is
// configured and no ReorgRecorder is, because a reorg detector whose only
// output is a log line that stops repeating after the recheck window leaves
// on-call nothing to act on. That refusal protects consumers who wire
// service.NewOnchain directly -- it does nothing for the facade unless
// EnableOnchain supplies the recorder, which is the line this test is about.
//
// Delete `ReorgRecorder: postgres.NewDepositReorgStore(s.pool)` from
// EnableOnchain and this test fails with that refusal.
func TestServiceEnableOnchain_WiresTheReorgRecorder(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	onchain, err := svc.EnableOnchain(wiringChainSet(), &wiringChainReader{}, nil, nil,
		// Long intervals: this test is about Run's startup gates, not about
		// any tick actually happening.
		service.WithWatchInterval(time.Hour),
		service.WithRecheckInterval(time.Hour),
		service.WithReorgRecheckInterval(time.Hour),
	)
	require.NoError(t, err)
	require.NotNil(t, onchain)

	// Pre-cancelled, like the service-side Run gate tests (F-m4): the gates
	// run before any loop starts, so cancelling cannot mask them, but a
	// regression comes back as one clean error instead of four background
	// loops blocking until the package -timeout.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = onchain.Run(ctx)
	require.NoError(t, err, "EnableOnchain must hand back an Onchain that Run accepts")
	if err != nil && strings.Contains(err.Error(), "ReorgRecorder") {
		t.Fatal("EnableOnchain did not wire a ReorgRecorder")
	}
}

// TestServiceEnableOnchain_WiresTheAdvisoryLockPool pins the second facade
// wiring line, and it is the one that was genuinely missing: EnableOnchain
// never passed service.WithPool(s.pool), and service.NewLockedJob treats a
// nil pool as "skip locking, run unconditionally". So the per-chain sweep
// lock -- and, as of B-m7, the per-chain forward-scan lock -- were
// implemented in service/ and inert for every consumer who went through this
// facade: multiple replicas broadcasting sweeps at the same nonce, and racing
// the forward-scan cursor whose standing still is how I-52 refuses to lose a
// deposit.
//
// The competing holder is a real service.LockedJob under the same job name,
// not a hand-computed hash: that way the test cannot drift away from the key
// derivation it is trying to collide with.
func TestServiceEnableOnchain_WiresTheAdvisoryLockPool(t *testing.T) {
	pool := postgrestest.SetupDB(t)
	svc, err := ledger.New(pool)
	require.NoError(t, err)

	reader := &wiringChainReader{}
	onchain, err := svc.EnableOnchain(wiringChainSet(), reader, nil, nil,
		service.WithWatchInterval(10*time.Millisecond),
		// The other loops must not muddy the call counts this test reads.
		// (The registration-rescan loop has no interval option, but it only
		// reaches the chain when a rescan job exists, and no deposit address
		// is ever registered here.)
		service.WithRecheckInterval(time.Hour),
		service.WithReorgRecheckInterval(time.Hour),
	)
	require.NoError(t, err)

	ctx := context.Background()

	// Occupy chain 1's watch lock through the production LockedJob path,
	// blocking inside the job so the lock stays held for the window below.
	held := make(chan struct{})
	release := make(chan struct{})
	holder := service.NewLockedJob("onchain_watch:1", func(context.Context) error {
		close(held)
		<-release
		return nil
	}, pool, core.NopLogger(), core.NopMetrics())
	holderDone := make(chan error, 1)
	go func() { holderDone <- holder.Run(ctx) }()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("test setup: the competing LockedJob never acquired the lock")
	}

	runCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	require.NoError(t, onchain.Run(runCtx))

	latestBlock, fetches := reader.calls()
	assert.Zero(t, latestBlock, "the watcher must not scan while another replica holds the per-chain lock")
	assert.Zero(t, fetches, "the watcher must not scan while another replica holds the per-chain lock")

	close(release)
	require.NoError(t, <-holderDone)

	// Positive contrast: an election that never elects anyone fails exactly
	// like no election at all, so the assertion above must be able to be
	// false (working-agreements §3).
	freeCtx, cancelFree := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelFree()
	require.NoError(t, onchain.Run(freeCtx))

	latestBlock, _ = reader.calls()
	assert.NotZero(t, latestBlock, "with the lock free the watcher must scan")
}
