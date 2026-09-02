package ledger_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger"
	"github.com/azex-ai/ledger/channel"
	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/service"
)

// ---------------------------------------------------------------------------
// The mechanical gate (I-40 property 4, generalised)
// ---------------------------------------------------------------------------

// cloneSafeMarker is what a method's doc comment must contain to opt out of
// the guard requirement below. It reads as a sentence in the godoc, so the
// declaration is visible to a consumer reading the API, not only to this test.
const cloneSafeMarker = "clone-safe:"

// cloneEscape describes one exported (*Service) method as the AST sees it.
type cloneEscape struct {
	name string
	// touchesPool: the body reads s.pool, i.e. it reaches past whatever
	// transaction the clone is bound to.
	touchesPool bool
	// writesFields: the body assigns to (or mutates through) s.<field>, i.e.
	// its effect lands on a value RunInTx discards when the callback returns.
	writesFields []string
	// guarded: the body branches on s.tx (any `s.tx != nil` / `s.tx == nil`
	// comparison), i.e. it makes a decision about being on a clone.
	guarded bool
	// declared: the doc comment carries cloneSafeMarker.
	declared bool
}

func (e cloneEscape) escapes() bool { return e.touchesPool || len(e.writesFields) > 0 }

// scanCloneEscapes walks a parsed file and classifies every exported method
// on *Service. src may be nil, in which case filename is read from disk.
func scanCloneEscapes(t *testing.T, filename, src string) []cloneEscape {
	t.Helper()

	fset := token.NewFileSet()
	var srcArg any
	if src != "" {
		srcArg = src
	}
	file, err := parser.ParseFile(fset, filename, srcArg, parser.ParseComments)
	require.NoError(t, err)

	var out []cloneEscape
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Body == nil {
			continue
		}
		star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		ident, ok := star.X.(*ast.Ident)
		if !ok || ident.Name != "Service" || !fn.Name.IsExported() {
			continue
		}
		if len(fn.Recv.List[0].Names) == 0 {
			continue // unnamed receiver cannot touch anything
		}
		recv := fn.Recv.List[0].Names[0].Name

		e := cloneEscape{name: fn.Name.Name}
		if fn.Doc != nil && strings.Contains(fn.Doc.Text(), cloneSafeMarker) {
			e.declared = true
		}

		isRecvField := func(expr ast.Expr) (string, bool) {
			sel, ok := expr.(*ast.SelectorExpr)
			if !ok {
				return "", false
			}
			x, ok := sel.X.(*ast.Ident)
			if !ok || x.Name != recv {
				return "", false
			}
			return sel.Sel.Name, true
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				if name, ok := isRecvField(v); ok && name == "pool" {
					e.touchesPool = true
				}
			case *ast.BinaryExpr:
				if name, ok := isRecvField(v.X); ok && name == "tx" {
					e.guarded = true
				}
				if name, ok := isRecvField(v.Y); ok && name == "tx" {
					e.guarded = true
				}
			case *ast.AssignStmt:
				for _, lhs := range v.Lhs {
					base := lhs
					if idx, ok := base.(*ast.IndexExpr); ok {
						base = idx.X // s.channels[name] = ...
					}
					if name, ok := isRecvField(base); ok {
						e.writesFields = append(e.writesFields, name)
					}
				}
			}
			return true
		})
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// TestCloneEscapeSurfaceIsDeclaredOrGuarded is the guardrail behind I-40
// property 4, replacing the hand-enumerated list of three methods that used
// to stand in for it.
//
// The enumeration failed the way every hand-maintained list in this
// repository has failed (README's API table, the sentinel-mapping table):
// AttestationService / VerifyLedger / EnableOnchain were guarded because
// somebody found them, while RegisterChannel silently discarded a
// registration it reported as successful, Worker built a pool-bound
// background worker out of a transaction, and Ping reported a healthy
// connection for a clone whose every read answered "tx is closed" -- all
// three the same shape, none on the list.
//
// The rule is a universal one instead: any exported (*Service) method whose
// body reaches past the clone's transaction -- by reading s.pool, or by
// writing a Service field that RunInTx throws away -- must either branch on
// s.tx (refuse, or rebind) or state its clone behaviour in its doc comment
// with a "clone-safe:" note. Adding a new method that touches s.pool turns
// this red until one of those two is true.
func TestCloneEscapeSurfaceIsDeclaredOrGuarded(t *testing.T) {
	methods := scanCloneEscapes(t, "ledger.go", "")

	// Guard against a vacuous pass: a detector that classified nothing would
	// satisfy every assertion below.
	require.Greater(t, len(methods), 40,
		"the scanner found almost no exported *Service methods -- it is not looking at ledger.go's real API surface")
	var escaping []string
	for _, m := range methods {
		if m.escapes() {
			escaping = append(escaping, m.name)
		}
	}
	require.GreaterOrEqual(t, len(escaping), 4,
		"the scanner found almost nothing reaching past the clone; ledger.go has at least Pool/Ping/Worker/RegisterChannel in that set (found: %v)", escaping)

	for _, m := range methods {
		if !m.escapes() || m.guarded || m.declared {
			continue
		}
		t.Errorf("(*Service).%s reaches past a RunInTx clone (touches s.pool=%v, writes %v) "+
			"but neither branches on s.tx nor declares its clone behaviour with a %q note in its "+
			"doc comment. On the clone RunInTx hands to its callback it will either operate outside "+
			"the caller's transaction or mutate a Service value that is discarded when the callback "+
			"returns -- both while reporting success. Add the guard, or add the note explaining why "+
			"the escape is intended.",
			m.name, m.touchesPool, m.writesFields, cloneSafeMarker)
	}
}

// TestCloneEscapeScanner_CatchesAnUnguardedUndeclaredMethod proves the gate
// above is falsifiable: the same scanner, pointed at a synthetic file with
// one unguarded pool-touching method and one field-writing method, reports
// both -- and reports neither for their guarded/declared counterparts.
// Without this, a scanner that never matched anything would let the gate pass
// forever (the "unfalsifiable conformance suite" shape I-48 calls out).
func TestCloneEscapeScanner_CatchesAnUnguardedUndeclaredMethod(t *testing.T) {
	const src = `package ledger

type Service struct{}

// Bare has no guard and no note.
func (s *Service) Bare() any { return s.pool }

// Declared reaches the pool on purpose.
//
// clone-safe: returns the pool by design.
func (s *Service) Declared() any { return s.pool }

func (s *Service) Guarded() any {
	if s.tx != nil {
		return nil
	}
	return s.pool
}

// Writer mutates a Service field with no guard.
func (s *Service) Writer() { s.onchain = 1 }

// Reader only reads a field.
func (s *Service) Reader() any { return s.onchain }
`

	got := map[string]cloneEscape{}
	for _, m := range scanCloneEscapes(t, "synthetic.go", src) {
		got[m.name] = m
	}
	require.Len(t, got, 5)

	require.True(t, got["Bare"].escapes())
	require.False(t, got["Bare"].guarded)
	require.False(t, got["Bare"].declared)

	require.True(t, got["Declared"].escapes())
	require.True(t, got["Declared"].declared, "the clone-safe note must be recognised in the doc comment")

	require.True(t, got["Guarded"].escapes())
	require.True(t, got["Guarded"].guarded, "an `s.tx != nil` branch must be recognised as a guard")

	require.Equal(t, []string{"onchain"}, got["Writer"].writesFields)
	require.True(t, got["Writer"].escapes(), "writing a Service field is an escape even without touching s.pool")

	require.False(t, got["Reader"].escapes(), "reading a field is not an escape -- the clone carries its own copy")
}

// ---------------------------------------------------------------------------
// Runtime pins for the three escapes the gate above found
// ---------------------------------------------------------------------------

type stubChannelAdapter struct{ name string }

func (a stubChannelAdapter) Name() string { return a.name }
func (a stubChannelAdapter) VerifySignature(http.Header, []byte) error {
	return nil
}
func (a stubChannelAdapter) ParseCallback(http.Header, []byte) (*channel.CallbackPayload, error) {
	return &channel.CallbackPayload{}, nil
}

// TestService_RegisterChannel_RefusedOnTxBoundClone pins the guard on
// RegisterChannel. Before it, the call returned nil -- reported success --
// and the registration landed in the clone's own map, which withTx copies and
// RunInTx discards: the transaction committed, the caller's own tables had
// their rows, and POST /api/v1/webhooks/<name> answered 404 forever with no
// error having been raised anywhere. This is literally the failure
// EnableOnchain's error message describes, on a method that had no guard.
//
// The top-level registration below is the control: it proves the same adapter
// is acceptable, so the refusal comes from the guard and not from the
// nil/empty-name/duplicate checks that follow it.
func TestService_RegisterChannel_RefusedOnTxBoundClone(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)

	require.NoError(t, svc.RegisterChannel(stubChannelAdapter{name: "top-level"}),
		"control: registering on the top-level Service must succeed")

	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		regErr := tx.RegisterChannel(stubChannelAdapter{name: "inside-tx"})
		require.Error(t, regErr, "RegisterChannel must be refused on a transaction-bound clone")
		require.ErrorIs(t, regErr, core.ErrInvalidInput)
		return nil
	})
	require.NoError(t, err)

	channels := svc.Channels()
	require.Contains(t, channels, "top-level")
	require.NotContains(t, channels, "inside-tx",
		"the refused registration must not have landed anywhere")
}

// TestService_Worker_RefusedOnTxBoundClone pins the guard on Worker. Worker
// was the only long-lived-object constructor on *Service without one, and the
// object it built from a clone was a chimera: the expiration service held
// stores bound to the transaction, everything else ran on the pool. Once the
// transaction committed, every expiration tick failed against a closed
// transaction and was swallowed by a log line, so expired reservations and
// bookings were never reclaimed again.
//
// It also closed a side door: Worker auto-wires an AttestationService built
// straight off s.pool -- the very object tx.AttestationService() refuses to
// hand out, asserted here on the same clone.
func TestService_Worker_RefusedOnTxBoundClone(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool, ledger.WithSilentWorker())
	require.NoError(t, err)

	// Control: the identical call succeeds on the top-level Service.
	topLevel, err := svc.Worker(service.DefaultWorkerConfig())
	require.NoError(t, err)
	require.NotNil(t, topLevel)

	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		w, workerErr := tx.Worker(service.DefaultWorkerConfig())
		require.Error(t, workerErr, "Worker must be refused on a transaction-bound clone")
		require.ErrorIs(t, workerErr, core.ErrInvalidInput)
		require.Nil(t, w, "no half-transactional Worker may escape the guard")
		return nil
	})
	require.NoError(t, err)
}

// TestService_Onchain_VisibleOnTxBoundClone pins the other half of the clone
// contract: refusing to CONFIGURE onchain from a clone (EnableOnchain's
// guard) is only coherent if READING it there still works. withTx used to
// drop the field, so a consumer writing the natural
// `tx.Onchain().IngestDeposit(...)` inside a callback nil-panicked on a
// Service whose top-level EnableOnchain had succeeded -- "configured equals
// not configured", the mirror image of the bug EnableOnchain's guard exists
// to prevent.
func TestService_Onchain_VisibleOnTxBoundClone(t *testing.T) {
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

	onchain, err := svc.EnableOnchain(chains, nil, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, onchain)

	err = svc.RunInTx(ctx, func(tx *ledger.Service) error {
		require.NotNil(t, tx.Onchain(),
			"the clone must see the onchain subsystem the top-level Service was given")
		require.Same(t, onchain, tx.Onchain(),
			"and it must be the same instance, not a second one built from the clone")
		return nil
	})
	require.NoError(t, err)
}

// TestService_Ping_FollowsTheCloneTransaction pins E-m14: a clone accidentally
// retained past its callback used to answer Ping with a healthy pool
// connection while every one of its data-plane calls answered "tx is closed".
// Ping now probes through DBTX(), so the health answer and the data answer
// agree.
func TestService_Ping_FollowsTheCloneTransaction(t *testing.T) {
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)

	svc, err := ledger.New(pool)
	require.NoError(t, err)
	require.NoError(t, svc.Ping(ctx), "control: the top-level Service is healthy")

	var escaped *ledger.Service
	require.NoError(t, svc.RunInTx(ctx, func(tx *ledger.Service) error {
		require.NoError(t, tx.Ping(ctx), "inside the callback the transaction is live, so Ping must succeed")
		escaped = tx
		return nil
	}))

	require.Error(t, escaped.Ping(ctx),
		"a clone retained past its callback must report unhealthy: every read and write it can still "+
			"be asked to do answers 'tx is closed', and Ping must not disagree with them")
	require.NoError(t, svc.Ping(ctx), "the top-level Service is unaffected")
}
