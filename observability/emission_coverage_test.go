package observability_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/azex-ai/ledger/core"
)

// TestEveryMetricsMethodHasAProductionCallSite is I-M1's pin: core.Metrics is
// a wide, one-signal-per-method interface specifically so a consumer can
// tell what each call site means without decoding a metric-name string --
// but that only holds if every method actually has at least one production
// call site. Before I-M1 landed, 12 of the (then) 32 methods had zero: the
// entire postgres/ write layer had no core.Metrics dependency at all.
//
// This parses service/ and postgres/ non-test .go source with go/ast and
// collects the method names actually CALLED on a metrics-shaped receiver,
// failing on any core.Metrics method that never appears.
//
// M-10 (W3 adversarial review of the gates): it used to concatenate those
// trees into one string and run `\.Name\(` over it, which counted comments
// and string literals. The reviewer appended a COMMENT containing
// `s.metrics.MutGauge(1)` and the coverage gate went green for a method with
// no call site at all. The file's own doc comment argued the false-positive
// direction was safe; for a COVERAGE gate it is the dangerous one -- it can
// only report emission that does not exist.
//
// Two things changed: only *ast.CallExpr counts (a comment is not a call),
// and the receiver must be metrics-shaped (`o.metrics()`, `s.metrics`,
// `deps.Metrics`), which also removes the old "coincidental same-named
// method on another type" caveat.
// crossBranchExclusions names core.Metrics methods D-ops (this task) added
// but whose call site lives in another Wave 2 task's exclusive files
// (contract §1/§4 file ownership) -- so it cannot be wired from this branch
// without violating that boundary. Each entry names who owns the call site;
// remove the entry once that branch lands its wiring, so this test starts
// enforcing it like every other method.
var crossBranchExclusions = map[string]string{
	// I-M1 (fleet-wide reserved-amount gauge): no existing query aggregates
	// reserved amount per-currency across all holders (only per-holder,
	// via SumActiveReservations) -- adding one is a new postgres/*.go query
	// beyond this task's "metrics/normalizeStoreError lines only" merge
	// constraint (contract §4's D-ops row). Left for a follow-up task with
	// its own migration/query budget.
	"ReservedAmount": "follow-up: needs a new per-currency reserved-amount query",
	// I-N12 (event delivery queue depth gauge): no existing query counts
	// pending/retry events -- same "new postgres/*.go query is out of this
	// task's merge budget" constraint as ReservedAmount above. The edge-
	// triggered counters this same finding asked for (EventDelivered/
	// EventDeliveryFailed/EventDead on LocalDispatcher) are wired; only the
	// backlog-depth gauge is deferred.
	"PendingEvents": "follow-up: needs a new CountPendingEvents query",
}

func TestEveryMetricsMethodHasAProductionCallSite(t *testing.T) {
	methods := metricsMethodNames(t)
	called := productionMetricsCalls(t)

	var missing []string
	for _, name := range methods {
		if _, excluded := crossBranchExclusions[name]; excluded {
			continue
		}
		if len(called[name]) == 0 {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("core.Metrics methods with zero production call sites under service/ or postgres/ (and not in crossBranchExclusions): %v\n\n"+
			"Note that a mention in a comment or a string is not a call site: this scan is AST-based (M-10).", missing)
	}
}

// productionMetricsCalls parses every non-test .go file under service/ and
// postgres/ and returns metrics method name -> the call sites that invoke it.
func productionMetricsCalls(t *testing.T) map[string][]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string][]string{}
	fset := token.NewFileSet()

	for _, dir := range []string{filepath.Join(root, "service"), filepath.Join(root, "postgres")} {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !isMetricsReceiver(sel.X) {
					return true
				}
				rel, _ := filepath.Rel(root, path)
				out[sel.Sel.Name] = append(out[sel.Sel.Name], rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}
	return out
}

// isMetricsReceiver reports whether an expression is the metrics handle:
// `o.metrics()`, `s.metrics`, `deps.Metrics`, or a bare `metrics` parameter.
// Anything else is some other type's identically named method, which this
// gate must not count.
func isMetricsReceiver(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.CallExpr:
		return isMetricsReceiver(v.Fun)
	case *ast.SelectorExpr:
		return strings.EqualFold(v.Sel.Name, "metrics")
	case *ast.Ident:
		return strings.EqualFold(v.Name, "metrics")
	default:
		return false
	}
}

// TestCrossBranchExclusionsAreStillActuallyMissing guards against
// crossBranchExclusions rotting into a silent permanent carve-out: once the
// owning branch lands its wiring, the method has a real call site and must
// be removed from the map so the main test starts enforcing it.
func TestCrossBranchExclusionsAreStillActuallyMissing(t *testing.T) {
	called := productionMetricsCalls(t)
	for name, owner := range crossBranchExclusions {
		if sites := called[name]; len(sites) > 0 {
			t.Errorf("core.Metrics.%s now has a production call site (%v, owner: %s) -- remove it from crossBranchExclusions in this file so TestEveryMetricsMethodHasAProductionCallSite enforces it", name, sites, owner)
		}
	}
}

// metricsMethodNames reflects core.Metrics to get the exact method set --
// keeping this test in sync with the interface automatically as methods are
// added (I-M1's own "core.Metrics 新方法 D-ops 独占追加权" clause means this
// list changes only here).
func metricsMethodNames(t *testing.T) []string {
	t.Helper()
	typ := reflect.TypeOf((*core.Metrics)(nil)).Elem()
	names := make([]string, typ.NumMethod())
	for i := range names {
		names[i] = typ.Method(i).Name
	}
	return names
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// This test file lives in observability/, one level below the repo root.
	return filepath.Dir(wd)
}

// untestedAlertMetrics maps a core.Metrics method with no behaviour pin
// anywhere to why it has none yet. M-10's second half (W3 adversarial review
// of the gates): the reviewer's census found four methods --
// DepositReorgDetected, DepositReviewRequired, RegistrationRescanFailed,
// SweepUnattributed -- referenced by no _test.go at all, which left
// TestEveryMetricsMethodHasAProductionCallSite as the only thing able to
// notice their emission disappearing. That gate answers "some source
// mentions it", never "this code path emits it", so for those four the answer
// was effectively nothing.
//
// Three now have pins (service/onchain_alert_metrics_test.go). This map is
// what keeps the fourth from being invisible, and what makes the next
// unpinned method a red build instead of a census somebody has to re-run by
// hand.
var untestedAlertMetrics = map[string]string{
	"SweepUnattributed": "emitted inside sweepTick, past a Scanner + Sweeper + gas-price + eligible-address fixture; needs the chains/evm e2e harness or a sizeable stub set. Tracked here rather than left silently uncovered",
	"PendingEvents":     "has no production call site at all yet (see crossBranchExclusions above) -- there is no path to pin until the backlog-depth query lands",
}

// TestEveryMetricsMethodHasABehaviourPin asserts that every core.Metrics
// method is named by at least one test file -- i.e. that something,
// somewhere, drives the code path and observes the emission -- or is
// registered above with the reason it does not.
//
// Naming a method in a test is weaker than proving the emission, and
// deliberately so: this is a census gate, and its job is to make the set of
// unpinned signals explicit and shrinking. The pins themselves live next to
// the code they exercise.
func TestEveryMetricsMethodHasABehaviourPin(t *testing.T) {
	named := metricsNamesInTests(t)

	var unpinned []string
	for _, name := range metricsMethodNames(t) {
		if _, known := untestedAlertMetrics[name]; known {
			continue
		}
		if !named[name] {
			unpinned = append(unpinned, name)
		}
	}
	sort.Strings(unpinned)
	if len(unpinned) > 0 {
		t.Errorf("core.Metrics method(s) named by no test file at all: %v\n\n"+
			"For these, the production call site can be deleted and only the coverage gate above could notice -- and that gate reads source, "+
			"not behaviour. Write a pin that drives the path and asserts the emission (service/onchain_alert_metrics_test.go and "+
			"service/worker_metrics_test.go are the shapes), or register it in untestedAlertMetrics with the reason it cannot be pinned yet.", unpinned)
	}

	// The register may only shrink: once a signal has a pin, it must leave.
	for name, reason := range untestedAlertMetrics {
		if named[name] {
			t.Errorf("core.Metrics.%s is now named by a test (registered as untested: %s) -- delete the untestedAlertMetrics entry so this gate enforces it", name, reason)
		}
	}
}

// metricsNamesInTests returns the metrics method names that appear as CODE
// in some _test.go file -- a method declaration, a call, or a field. Parsed,
// not grepped, for the same reason the coverage gate above is: this file and
// service/onchain_alert_metrics_test.go both discuss these names in prose,
// and a census satisfied by its own comments is no census (M-10).
//
// Skipped: core/metrics_embed_test.go, which enumerates the interface by
// reflection rather than exercising any path, and would name every method
// for free.
func metricsNamesInTests(t *testing.T) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	out := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") || filepath.Base(path) == "metrics_embed_test.go" {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.SelectorExpr:
				out[v.Sel.Name] = true
			case *ast.FuncDecl:
				out[v.Name.Name] = true
			case *ast.Ident:
				out[v.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("parse test sources under %s: %v", root, err)
	}
	return out
}
