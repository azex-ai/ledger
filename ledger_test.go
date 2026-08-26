package ledger_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/authdev"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// ---------------------------------------------------------------------------
// NewIdempotencyKey
// ---------------------------------------------------------------------------

func TestNewIdempotencyKey_Format(t *testing.T) {
	scope := "deposit"
	key := ledger.NewIdempotencyKey(scope)

	// Must start with the scope followed by a colon.
	if !strings.HasPrefix(key, scope+":") {
		t.Fatalf("expected key to start with %q, got %q", scope+":", key)
	}

	// Suffix must be 32 hex characters (16 bytes).
	suffix := strings.TrimPrefix(key, scope+":")
	if len(suffix) != 32 {
		t.Fatalf("expected 32-char hex suffix, got len=%d: %q", len(suffix), suffix)
	}
	for _, c := range suffix {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("non-hex character %q in suffix %q", c, suffix)
		}
	}
}

func TestNewIdempotencyKey_Unique(t *testing.T) {
	// Generate 1000 keys and verify all are unique.
	const n = 1000
	seen := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		k := ledger.NewIdempotencyKey("test")
		if _, dup := seen[k]; dup {
			t.Fatalf("duplicate idempotency key generated: %q", k)
		}
		seen[k] = struct{}{}
	}
}

func TestNewIdempotencyKey_EmptyScope(t *testing.T) {
	key := ledger.NewIdempotencyKey("")
	// With an empty scope the key starts with ":"
	if !strings.HasPrefix(key, ":") {
		t.Fatalf("expected key to start with ':', got %q", key)
	}
}

func TestNewIdempotencyKey_SpecialCharactersInScope(t *testing.T) {
	scope := "my-scope/v2"
	key := ledger.NewIdempotencyKey(scope)
	if !strings.HasPrefix(key, scope+":") {
		t.Fatalf("expected key to start with %q, got %q", scope+":", key)
	}
}

// ---------------------------------------------------------------------------
// Ping — unit test (no real DB; only checks nil-pool fast-fail path)
// ---------------------------------------------------------------------------

func TestService_Ping_NilPool(t *testing.T) {
	_, err := ledger.New(nil)
	if err == nil {
		t.Fatal("expected error when pool is nil, got nil")
	}
}

// TestService_Ping_Integration is intentionally skipped when no DB is
// available — the testcontainers integration suite covers the live path.
func TestService_Ping_Integration(t *testing.T) {
	t.Skip("requires PostgreSQL; covered by postgres integration tests")
	_ = context.Background()
}

// ---------------------------------------------------------------------------
// EnableOnchain — MJ1 secure-by-default fence, facade layer
// (docs/bugs/2026-07-11-m3-security-review.md)
// ---------------------------------------------------------------------------

// newNoDialPool builds a *pgxpool.Pool that never actually connects
// (pgxpool.New connects lazily) — sufficient here, since these tests only
// exercise EnableOnchain's in-memory config validation and never issue a
// query.
func newNoDialPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://user:pass@127.0.0.1:1/nonexistent")
	if err != nil {
		t.Fatalf("unexpected error constructing pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func unconfiguredChainSet() core.ChainSet {
	return core.ChainSet{
		1: {
			ChainID:  1,
			Factory:  "0x6CE5E7A510C693E1E4FC032d8De0c394C9C1A323",
			InitHash: "0x2ef28d391fa40901fc8c61168ece13f5247e49e87925cd7f617262b9231b9ec",
			CreditTokens: map[string]core.TokenConfig{
				// AutoCreditCeiling deliberately left at its zero value.
				"0xusdt": {TokenAddress: "0xusdt", CurrencyCode: "USDT"},
			},
		},
	}
}

// TestService_EnableOnchain_RejectsUnconfiguredAutoCreditCeiling pins MJ1's
// facade-layer closure: a consumer that wires Onchain via EnableOnchain and
// never calls service.Onchain.Run at all (e.g. a push-only/webhook-only
// deployment driving IngestDeposit straight from an HTTP handler, with no
// background jobs to justify calling Run) must still be refused an
// unvalidated instance — Run()'s own check alone is not enough to close
// this path.
func TestService_EnableOnchain_RejectsUnconfiguredAutoCreditCeiling(t *testing.T) {
	pool := newNoDialPool(t)
	svc, err := ledger.New(pool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	onchain, err := svc.EnableOnchain(unconfiguredChainSet(), nil, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if onchain != nil {
		t.Fatal("expected a nil *service.Onchain on validation failure")
	}
	if svc.Onchain() != nil {
		t.Fatal("Service.Onchain() must stay nil after a failed EnableOnchain call")
	}
	if !strings.Contains(err.Error(), "AutoCreditCeiling") {
		t.Fatalf("expected error to mention AutoCreditCeiling, got: %v", err)
	}
}

// TestService_EnableOnchain_AllowsExplicitUnboundedSentinel pins MJ1's
// escape hatch at the facade layer: a consumer that deliberately sets
// AutoCreditCeiling to core.UnboundedAutoCredit is not blocked by the same
// check.
func TestService_EnableOnchain_AllowsExplicitUnboundedSentinel(t *testing.T) {
	pool := newNoDialPool(t)
	svc, err := ledger.New(pool)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	chains := unconfiguredChainSet()
	cfg := chains[1]
	tc := cfg.CreditTokens["0xusdt"]
	tc.AutoCreditCeiling = core.UnboundedAutoCredit
	cfg.CreditTokens["0xusdt"] = tc
	chains[1] = cfg

	onchain, err := svc.EnableOnchain(chains, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if onchain == nil {
		t.Fatal("expected a non-nil *service.Onchain")
	}
	if svc.Onchain() == nil {
		t.Fatal("Service.Onchain() must be set after a successful EnableOnchain call")
	}
}

// ---------------------------------------------------------------------------
// Worker — P6 batch attestation auto-wiring
// ---------------------------------------------------------------------------

// TestServiceWorker_AttestsAutomaticallyWhenAttestorConfigured pins the fix
// to the audit's most-confirmed finding on this facade: Worker.SetAttestor
// had zero production call sites anywhere in this repository, and
// (*Service).Worker never called it either, even though
// service.DefaultWorkerConfig configures an AttestInterval as if the job
// were always going to run. A consumer that built a Service WithAttestor and
// ran svc.Worker(cfg) got every journal signed (that part worked) but the
// P6 batch attestation chain -- the thing that turns per-journal signatures
// into a tamper-evident, externally-anchorable chain -- never advanced a
// single seq.
//
// This test goes through the exact facade a consumer calls (ledger.New +
// WithAttestor, then svc.Worker(cfg).Run) and makes no direct call to
// worker.SetAttestor. Deleting the auto-wiring this fix adds inside
// (*Service).Worker must turn this test red -- if it does not, the test is
// making the same mistake the audit called out in Worker.Subscribe's
// original regression tests (service/worker_subscribe_test.go): protecting
// a wiring call the test itself performed, not the one a real consumer
// gets.
func TestServiceWorker_AttestsAutomaticallyWhenAttestorConfigured(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "worker-attest-test-key")
	require.NoError(t, err)

	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	// Minimal fixture: one currency, one classification, one journal type --
	// enough to post a single balanced journal through the ordinary facade
	// path (svc.JournalWriter().PostJournal), which is what the batch
	// attestation job needs something to attest.
	suffix := time.Now().UnixNano()
	cur, err := svc.Currencies().CreateCurrency(ctx, core.CurrencyInput{
		Code: fmt.Sprintf("WAC_%d", suffix), Name: "Worker Attest Currency", Exponent: 18,
	})
	require.NoError(t, err)
	cls, err := svc.Classifications().CreateClassification(ctx, core.ClassificationInput{
		Code: fmt.Sprintf("watt_main_%d", suffix), Name: "Worker Attest Main", NormalSide: core.NormalSideDebit,
		BalanceRole: core.BalanceRoleAvailable,
	})
	require.NoError(t, err)
	jt, err := svc.JournalTypes().CreateJournalType(ctx, core.JournalTypeInput{
		Code: fmt.Sprintf("watt_jt_%d", suffix), Name: "Worker Attest JT",
	})
	require.NoError(t, err)

	holder := int64(9202)
	_, err = svc.JournalWriter().PostJournal(ctx, core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: postgrestest.UniqueKey("worker-attest"),
		Source:         "worker-attest-test",
		ActorID:        holder,
		Entries: []core.EntryInput{
			{AccountHolder: holder, CurrencyUID: cur.UID, ClassificationUID: cls.UID, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(1)},
			{AccountHolder: core.SystemAccountHolder(holder), CurrencyUID: cur.UID, ClassificationUID: cls.UID, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(1)},
		},
	})
	require.NoError(t, err)

	cfg := service.DefaultWorkerConfig()
	cfg.AttestInterval = 50 * time.Millisecond
	cfg.AttestBatchSize = 100
	// The only call a consumer makes -- no worker.SetAttestor.
	worker := svc.Worker(cfg)

	workerCtx, cancelWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.Run(workerCtx) }()
	t.Cleanup(func() {
		cancelWorker()
		<-workerDone
	})

	require.Eventually(t, func() bool {
		var seq int64
		if err := pool.QueryRow(ctx, "SELECT COALESCE(MAX(seq), 0) FROM ledger_attestations").Scan(&seq); err != nil {
			return false
		}
		return seq > 0
	}, 5*time.Second, 50*time.Millisecond,
		"the P6 batch attestation chain never advanced within 5s -- ledger.Service.Worker "+
			"is not wiring SetAttestor automatically even though this Service was constructed "+
			"WithAttestor. This is the exact regression the auto-wiring exists to prevent.")
}

// TestService_Worker_ConcurrentCallsDoNotRaceOnEventStore pins that
// (*Service).Worker no longer mutates the Service's own shared EventStore.
// Before this fix, every call did `s.eventStore.SetClaimLease(...)` on the
// one *postgres.EventStore instance also returned by EventReader() and by
// every other Worker() call on this Service -- a plain, unsynchronized field
// write. Run with `go test -race` (as `make test` always does), N goroutines
// each calling svc.Worker(cfg) with a different EventClaimLease reliably hit
// that field concurrently and the race detector flags it deterministically,
// no timing luck required. It is also a real behavioural bug independent of
// -race: the last Worker() call's lease silently became every earlier
// Worker() call's lease too, because they all pointed at the identical
// EventStore. This test's job is the concurrent-write shape; the isolation
// fix (each Worker() call gets its own EventStore) removes the shared state
// the race depends on entirely.
func TestService_Worker_ConcurrentCallsDoNotRaceOnEventStore(t *testing.T) {
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	const n = 8
	done := make(chan struct{})
	for i := 0; i < n; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			cfg := service.DefaultWorkerConfig()
			cfg.EventClaimLease = time.Duration(i+1) * time.Second
			_ = svc.Worker(cfg)
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
}

// ---------------------------------------------------------------------------
// RunInTx — nesting guard, and facade methods refused on a tx-bound clone
// ---------------------------------------------------------------------------

// TestService_RunInTx_NestedCallIsRejected pins that calling RunInTx again on
// the *Service handed to an outer RunInTx callback returns an error instead
// of silently opening a second, independent transaction from the pool.
// Before this fix, RunInTxWithOptions unconditionally called s.pool.BeginTx
// regardless of whether s was already transaction-bound, so the inner
// transaction committed or rolled back completely independently of the
// outer one -- defeating the atomicity RunInTx exists to provide, and,
// where the two transactions want the same advisory-locked balance,
// deadlocking instead.
func TestService_RunInTx_NestedCallIsRejected(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	outerRan := false
	innerRan := false
	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		outerRan = true
		return tx.RunInTx(ctx, func(inner *ledger.Service) error {
			innerRan = true
			return nil
		})
	})
	require.Error(t, err, "a nested RunInTx call must be rejected, not silently open an independent transaction")
	require.True(t, outerRan, "the outer callback must still have run")
	require.False(t, innerRan, "the inner callback must never run once nesting is rejected")
}

// TestService_AttestationService_RefusedOnTxBoundClone pins that
// AttestationService cannot be built from the clone RunInTx hands to its
// callback: that service reads/writes ledger_attestations through the pool
// directly (batch-scoped, spanning many entries), which would silently
// operate outside whatever transaction the caller thinks it is composing
// with if allowed to run from inside RunInTx.
func TestService_AttestationService_RefusedOnTxBoundClone(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "runintx-escape-test-key")
	require.NoError(t, err)

	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		_, asErr := tx.AttestationService(nil)
		require.Error(t, asErr, "AttestationService must be refused on a transaction-bound clone")
		return nil
	})
	require.NoError(t, err)
}

// TestService_VerifyLedger_NotRunOnTxBoundClone pins that VerifyLedger
// fail-closes to NOT_RUN (never a partial VERIFIED) when called on a
// transaction-bound clone, for the same reason as AttestationService above:
// it would otherwise mix a pool-level read of the attestation chain with the
// clone's transactional view of everything else.
//
// anchor and verifier are both real (not nil) here, deliberately: passing
// nil for either would trip VerifyLedger's own pre-existing "no
// Anchor/AuthVerifier configured" NOT_RUN branches (service/attest_verify.go)
// and pass even without the s.tx guard this test exists to protect --
// exactly the "test protects a wiring it performed itself" trap
// service/worker_subscribe_test.go's original four tests fell into. With a
// real anchor/verifier and nothing yet attested, VerifyLedger would
// otherwise complete and report VERIFIED (zero seqs is a vacuously
// consistent chain); this test only proves NOT_RUN if that completion is
// what the guard interrupts.
func TestService_VerifyLedger_NotRunOnTxBoundClone(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	attestor, verifier, err := authdev.NewLocalAttestor(priv.Seed(), "verify-tx-escape-test-key")
	require.NoError(t, err)
	anchor := anchordev.NewLocalFileAnchor(filepath.Join(t.TempDir(), "anchor.txt"))

	svc, err := ledger.New(pool, ledger.WithAttestor(attestor, verifier))
	require.NoError(t, err)

	// Sanity: on the top-level Service (no tx), with nothing attested yet,
	// this same call completes and reports VERIFIED -- establishing that a
	// NOT_RUN result from the tx-bound clone below comes from the guard,
	// not from some other reason this configuration would always NOT_RUN.
	topLevel := svc.VerifyLedger(ctx, anchor, service.VerifyConfig{})
	require.Equal(t, service.VerifyStatusVerified, topLevel.Status,
		"top-level sanity check: VerifyLedger with a real anchor/verifier and nothing attested yet should report VERIFIED, got %+v", topLevel)

	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		report := tx.VerifyLedger(ctx, anchor, service.VerifyConfig{})
		require.Equal(t, service.VerifyStatusNotRun, report.Status,
			"VerifyLedger on a tx-bound clone must fail-closed to NOT_RUN, not attempt a mixed-view check")
		return nil
	})
	require.NoError(t, err)
}

// TestService_EnableOnchain_RefusedOnTxBoundClone pins that EnableOnchain is
// refused on a transaction-bound clone. Before this fix it would set
// s.onchain on the short-lived clone RunInTx discards when the callback
// returns, so the call reported success while svc.Onchain() stayed nil on
// the top-level Service -- silently discarding the configuration the caller
// just "successfully" installed.
//
// Uses the explicit-unbounded-sentinel ChainSet
// (TestService_EnableOnchain_AllowsExplicitUnboundedSentinel's fixture),
// deliberately, not unconfiguredChainSet(): the latter fails
// ValidateAutoCreditCeilings regardless of which Service it is called on,
// which would pass this test for the wrong reason (the same "test protects
// a wiring it performed itself" trap as the VerifyLedger test above).
func TestService_EnableOnchain_RefusedOnTxBoundClone(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	chains := unconfiguredChainSet()
	cfg := chains[1]
	tc := cfg.CreditTokens["0xusdt"]
	tc.AutoCreditCeiling = core.UnboundedAutoCredit
	cfg.CreditTokens["0xusdt"] = tc
	chains[1] = cfg

	// Sanity: the same ChainSet succeeds on the top-level Service.
	topLevelOnchain, err := svc.EnableOnchain(chains, nil, nil, nil)
	require.NoError(t, err, "top-level sanity check: this ChainSet must be valid enough to succeed outside RunInTx")
	require.NotNil(t, topLevelOnchain)
	require.NotNil(t, svc.Onchain())

	// A second Service, fresh, so the tx-bound call below is refused by the
	// guard under test -- not by the already-configured guard on svc.
	svc2, err := ledger.New(pool)
	require.NoError(t, err)

	err = svc2.RunInTx(ctx, func(tx *ledger.Service) error {
		_, enableErr := tx.EnableOnchain(chains, nil, nil, nil)
		require.Error(t, enableErr, "EnableOnchain must be refused on a transaction-bound clone")
		return nil
	})
	require.NoError(t, err)
	require.Nil(t, svc2.Onchain(), "Service.Onchain() must stay nil: EnableOnchain was never accepted on the top-level Service")
}
