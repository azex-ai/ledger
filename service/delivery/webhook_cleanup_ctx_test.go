package delivery

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// ctxCheckingPoller refuses a bookkeeping call that arrives on a dead
// context, the way a real store does: pgx checks ctx.Err() before touching
// the wire, so a cancelled ctx means the UPDATE never happens.
type ctxCheckingPoller struct {
	events []PendingEvent

	delivered int
	retried   int
	lastErr   error
}

func (p *ctxCheckingPoller) GetPendingEvents(_ context.Context, _ int) ([]PendingEvent, error) {
	// Deliberately ignores ctx: in a real run the batch was already claimed,
	// and this test is about what happens to the bookkeeping afterwards.
	return p.events, nil
}

func (p *ctxCheckingPoller) MarkDelivered(ctx context.Context, _ int64, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	p.delivered++
	return nil
}

func (p *ctxCheckingPoller) MarkRetry(ctx context.Context, _ int64, _ time.Time, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	p.retried++
	return nil
}

func (p *ctxCheckingPoller) MarkDead(ctx context.Context, _ int64, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		p.lastErr = err
		return err
	}
	return nil
}

var _ EventPoller = (*ctxCheckingPoller)(nil)

// ctxCheckingSubscribers is the SubscriberLister half of the same idea.
type ctxCheckingSubscribers struct {
	subs []WebhookSubscriber
	// onRecord runs at the start of RecordDeliveryStatus, i.e. after the
	// outbound POST has completed. It is how the mid-flight-shutdown test
	// cancels the parent at exactly the moment a real shutdown would be
	// indistinguishable from a lost delivery.
	onRecord func()

	recorded int
	lastErr  error
}

func (s *ctxCheckingSubscribers) ListActiveSubscribers(_ context.Context) ([]WebhookSubscriber, error) {
	return s.subs, nil
}

func (s *ctxCheckingSubscribers) RecordDeliveryStatus(ctx context.Context, _ int64, _ int, _ string) error {
	if s.onRecord != nil {
		s.onRecord()
	}
	if err := ctx.Err(); err != nil {
		s.lastErr = err
		return err
	}
	s.recorded++
	return nil
}

var _ SubscriberLister = (*ctxCheckingSubscribers)(nil)

func onePendingWebhookEvent() []PendingEvent {
	return []PendingEvent{{
		Event: core.Event{
			UID:                "22222222-2222-2222-2222-222222222222",
			ClassificationCode: "deposit",
			ToStatus:           core.Status("confirmed"),
			MaxAttempts:        5,
		},
		InternalID: 42,
		ClaimToken: time.Now(),
	}}
}

// TestWebhookDeliverer_FilteredOutMarkSurvivesCancelledParent covers the
// "every subscriber's filter declined this event" branch: that IS a completed
// delivery decision (ProcessBatch's own comment says so), so it must be
// recorded even when the parent context died between the poll and the mark --
// worker shutdown mid-batch. Passing the cancelled ctx straight through, what
// this file used to do, loses the decision and lets the lease expire into a
// redelivery.
func TestWebhookDeliverer_FilteredOutMarkSurvivesCancelledParent(t *testing.T) {
	poller := &ctxCheckingPoller{events: onePendingWebhookEvent()}
	subs := &ctxCheckingSubscribers{subs: []WebhookSubscriber{{
		ID: 1, Name: "other", URL: "http://127.0.0.1:1/never",
		FilterClass: "withdrawal", IsActive: true,
	}}}
	d := NewWebhookDeliverer(poller, subs, core.NopLogger(), core.NopMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := d.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, poller.delivered,
		"MarkDelivered must run on a detached context (last ctx error: %v)", poller.lastErr)
}

// TestWebhookDeliverer_FailureBookkeepingSurvivesCancelledParent covers the
// other two sites at once. With a dead parent the outbound POST cannot
// succeed -- which is correct, sendHTTP deliberately keeps the original ctx
// because a shutdown SHOULD abort an in-flight request -- but the two things
// that record what already happened must still land: the subscriber's health
// record (lose it and a failing endpoint looks healthy) and the retry
// schedule (lose it and the attempt count never advances, so a permanently
// broken subscriber is retried at the lease interval forever instead of
// reaching max_attempts).
func TestWebhookDeliverer_FailureBookkeepingSurvivesCancelledParent(t *testing.T) {
	poller := &ctxCheckingPoller{events: onePendingWebhookEvent()}
	subs := &ctxCheckingSubscribers{subs: []WebhookSubscriber{{
		ID: 1, Name: "sub", URL: "http://127.0.0.1:1/unreachable",
		Secret: "s3cret", IsActive: true,
	}}}
	d := NewWebhookDeliverer(poller, subs, core.NopLogger(), core.NopMetrics())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, subs.recorded,
		"RecordDeliveryStatus must run on a detached context (last ctx error: %v)", subs.lastErr)
	assert.Equal(t, 1, poller.retried,
		"MarkRetry must run on a detached context (last ctx error: %v)", poller.lastErr)
}

// TestWebhookDeliverer_SuccessMarkSurvivesShutdownMidFlight is the fourth
// site: every matched subscriber answered 200 and THEN the worker was told to
// stop. A pre-cancelled parent cannot reach this branch (the POST would never
// complete), so the cancellation is fired from inside RecordDeliveryStatus --
// which runs immediately after sendHTTP returns, so it is exactly the window
// in which a shutdown makes "delivered" and "lost" indistinguishable.
func TestWebhookDeliverer_SuccessMarkSurvivesShutdownMidFlight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	poller := &ctxCheckingPoller{events: onePendingWebhookEvent()}
	subs := &ctxCheckingSubscribers{
		subs: []WebhookSubscriber{{
			ID: 1, Name: "sub", URL: srv.URL, Secret: "s3cret", IsActive: true,
		}},
		onRecord: cancel,
	}
	d := NewWebhookDeliverer(poller, subs, core.NopLogger(), core.NopMetrics())

	n, err := d.ProcessBatch(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, subs.recorded, "the delivery must have been recorded (last ctx error: %v)", subs.lastErr)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, poller.delivered,
		"MarkDelivered must run on a detached context: the event reached every matched subscriber before the shutdown, and dropping the mark redelivers an event that landed (last ctx error: %v)", poller.lastErr)
}

// bookkeepingCalls are the calls in this package that record something that
// has ALREADY happened. Every one of them must run on a cleanupContext, and
// the rule is stated in cleanup_context.go's doc comment.
//
// sendHTTP and GetPendingEvents are deliberately absent: aborting an
// in-flight outbound request, or a poll that has not claimed anything yet, is
// the correct response to a shutdown.
var bookkeepingCalls = map[string]bool{
	"MarkDelivered":        true,
	"MarkRetry":            true,
	"MarkDead":             true,
	"RecordDeliveryStatus": true,
}

// pendingAttachedBookkeeping is the shrink-only registry of call sites known
// to still pass the live ctx, keyed by file and method.
//
// It is empty, and it earned the right to be: it held local.go's two sites
// while their fix sat on the D-lock branch (that task's exclusive face, so
// they were reported here rather than edited), and when D-lock merged the
// gate went red naming both -- "now detached, delete it from the registry" --
// instead of quietly tolerating them forever. That is what "red in both
// directions" buys: an entry that is not listed must be detached, and a
// listed one that has BECOME detached must be deleted in the same commit, so
// the registry can only shrink to nothing (contracts §8's rule for advisory
// sets: explicit, shrink-only).
//
// Keep it, empty, for the next hand-off across a package boundary.
var pendingAttachedBookkeeping = map[string]map[string]bool{}

// TestDeliveryBookkeeping_AlwaysUsesCleanupContext is the structural half.
//
// The behavioural pins above prove the four call sites that exist today are
// detached. This one proves the NEXT one will be, which is the part a
// per-branch test cannot do: the defect d-lock found (concurrency.md B-m2)
// survived because the rule lived only in a doc comment, and
// working-agreements §5 says a rule that can be machine-checked must not be
// left to memory.
//
// It fails closed: if the scan finds fewer call sites than exist today it
// reports that instead of passing, so a scanner that stops matching (a
// renamed method, a moved file, a refactor into a helper) cannot read as a
// clean run.
func TestDeliveryBookkeeping_AlwaysUsesCleanupContext(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	found := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ParseComments)
		require.NoError(t, err)

		// Idents bound to a cleanupContext result anywhere in this file.
		detached := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if fn, ok := call.Fun.(*ast.Ident); !ok || fn.Name != "cleanupContext" {
				return true
			}
			if id, ok := as.Lhs[0].(*ast.Ident); ok {
				detached[id.Name] = true
			}
			return true
		})

		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !bookkeepingCalls[sel.Sel.Name] {
				return true
			}
			found++

			pos := fset.Position(call.Lparen)
			arg, isIdent := call.Args[0].(*ast.Ident)
			isDetached := isIdent && detached[arg.Name]
			pending := pendingAttachedBookkeeping[name][sel.Sel.Name]

			switch {
			case !isDetached && !pending:
				t.Errorf("%s:%d: %s is bookkeeping for something that already happened, "+
					"so it must run on a cleanupContext(ctx) result, not on the ctx that may have just been "+
					"cancelled (see cleanup_context.go). Got first argument %s.",
					name, pos.Line, sel.Sel.Name, ctxArgString(call.Args[0]))
			case isDetached && pending:
				t.Errorf("%s:%d: %s is now detached, so delete it from pendingAttachedBookkeeping "+
					"in the same commit -- the registry may only shrink.", name, pos.Line, sel.Sel.Name)
			}
			return true
		})
	}

	// Today: webhook.go has four (MarkDelivered x2, RecordDeliveryStatus,
	// MarkRetry) and local.go two (MarkDelivered, MarkRetry).
	require.GreaterOrEqual(t, found, 6,
		"the scan found only %d bookkeeping call sites; it must not report a clean run when it has stopped seeing them", found)
}

func ctxArgString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.CallExpr:
		return "a call expression"
	default:
		return "a non-identifier expression"
	}
}
