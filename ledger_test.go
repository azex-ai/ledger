package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/core"
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
