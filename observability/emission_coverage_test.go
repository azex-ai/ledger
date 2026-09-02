package observability_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
// This walks service/ and postgres/ non-test .go source (mechanical scan --
// see contract §0's "grep the shape across the repo, not just the TODO's one
// spot") looking for `.<MethodName>(` and fails on any core.Metrics method
// name that never appears. It cannot tell a real call from a coincidental
// substring match on an unrelated method of the same name on a different
// type -- that false-positive direction is safe (it can only make the test
// pass when it should not, never fail spuriously) and no method name here
// collides with anything else in the two scanned trees as of this writing.
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

	root := repoRoot(t)
	src := scanNonTestGoSource(t, filepath.Join(root, "service"))
	src += scanNonTestGoSource(t, filepath.Join(root, "postgres"))

	var missing []string
	for _, name := range methods {
		if _, excluded := crossBranchExclusions[name]; excluded {
			continue
		}
		pattern := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\(`)
		if !pattern.MatchString(src) {
			missing = append(missing, name)
		}
	}

	if len(missing) > 0 {
		t.Fatalf("core.Metrics methods with zero production call sites under service/ or postgres/ (and not in crossBranchExclusions): %v", missing)
	}
}

// TestCrossBranchExclusionsAreStillActuallyMissing guards against
// crossBranchExclusions rotting into a silent permanent carve-out: once the
// owning branch lands its wiring, the method has a real call site and must
// be removed from the map so the main test starts enforcing it.
func TestCrossBranchExclusionsAreStillActuallyMissing(t *testing.T) {
	root := repoRoot(t)
	src := scanNonTestGoSource(t, filepath.Join(root, "service"))
	src += scanNonTestGoSource(t, filepath.Join(root, "postgres"))

	for name, owner := range crossBranchExclusions {
		pattern := regexp.MustCompile(`\.` + regexp.QuoteMeta(name) + `\(`)
		if pattern.MatchString(src) {
			t.Errorf("core.Metrics.%s now has a production call site (owner: %s) -- remove it from crossBranchExclusions in this file so TestEveryMetricsMethodHasAProductionCallSite enforces it", name, owner)
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

func scanNonTestGoSource(t *testing.T, dir string) string {
	t.Helper()
	var sb strings.Builder
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sb.Write(b)
		sb.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", dir, err)
	}
	return sb.String()
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
