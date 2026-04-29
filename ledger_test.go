package ledger_test

import (
	"context"
	"strings"
	"testing"

	"github.com/azex-ai/ledger"
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
