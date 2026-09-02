package observability_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/observability"
)

// exerciseEveryMethod calls every core.Metrics method once with a
// zero-shaped-but-valid argument, via reflection over the interface rather
// than a hand-maintained call list (mirrors ledger.go's mergeWorkerConfig
// reflection pattern: adding a method here is then automatic, not another
// place someone has to remember to update). This is required, not
// cosmetic: a *prometheus.CounterVec/GaugeVec/HistogramVec has NO child
// time series -- and so does not appear in Registry().Gather() at all --
// until WithLabelValues is called at least once. A never-called Vec metric
// would otherwise silently read as "not registered" by this test's own
// registeredMetricNames helper, which is exactly backwards from this file's
// purpose.
func exerciseEveryMethod(m core.Metrics) {
	v := reflect.ValueOf(m)
	typ := reflect.TypeOf((*core.Metrics)(nil)).Elem()
	for i := 0; i < typ.NumMethod(); i++ {
		method := typ.Method(i)
		mv := v.MethodByName(method.Name)
		args := make([]reflect.Value, method.Type.NumIn())
		for j := range args {
			args[j] = zeroArgFor(method.Type.In(j))
		}
		mv.Call(args)
	}
}

// zeroArgFor returns a representative argument value for t. Every
// core.Metrics parameter type is one of: string, bool, int, int64, int32,
// time.Duration, or decimal.Decimal -- reflect.Zero(t) is fine for all of
// them except decimal.Decimal, whose zero value must be constructed via its
// own constructor (its zero struct value is not a valid decimal.Decimal for
// string formatting in some shopspring/decimal versions).
func zeroArgFor(t reflect.Type) reflect.Value {
	if t == reflect.TypeOf(decimal.Decimal{}) {
		return reflect.ValueOf(decimal.Zero)
	}
	if t == reflect.TypeOf(time.Duration(0)) {
		return reflect.ValueOf(time.Second)
	}
	switch t.Kind() {
	case reflect.String:
		return reflect.ValueOf("x")
	default:
		return reflect.Zero(t)
	}
}

// registeredMetricNames builds a NewPrometheusMetrics(), exercises every
// core.Metrics method on it (see exerciseEveryMethod), then scrapes the
// registry -- returning the set of metric names it actually exports. This
// is the single source of truth every doc reference in this test is checked
// against (I-M2: "把清单的唯一真相源留在代码里").
func registeredMetricNames(t *testing.T) map[string]bool {
	t.Helper()
	m := observability.NewPrometheusMetrics()
	exerciseEveryMethod(m)
	mfs, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("gather registry: %v", err)
	}
	names := make(map[string]bool, len(mfs))
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	return names
}

// ledgerMetricTokenPattern matches a bare ledger_* token in prose or a code
// block. The `ledger_` prefix alone is not unique to metrics in this repo --
// it is also the DB role prefix (`ledger_app`, `ledger_owner`, `ledger_ro`),
// several SECURITY DEFINER trigger/function names (`ledger_block_mutation`,
// `ledger_signed_amount`), and appears in Go filenames (`ledger_store.go`)
// -- so candidates are filtered by suffix in suffixesLikelyMetric below
// before being checked against the registry.
var ledgerMetricTokenPattern = regexp.MustCompile(`ledger_[a-z_]+`)

// metricSuffixesFromRegistry returns the set of final underscore-delimited
// words across every currently-registered metric name (e.g. "total",
// "seconds", "units", "blocks", "active", "pending", "stuck", "seqs",
// "count") -- derived from the registry itself, not hand-maintained, so a
// newly added metric with a new suffix shape is picked up automatically
// without touching this test.
func metricSuffixesFromRegistry(registered map[string]bool) map[string]bool {
	suffixes := make(map[string]bool)
	for name := range registered {
		parts := regexp.MustCompile(`_`).Split(name, -1)
		if len(parts) > 0 {
			suffixes[parts[len(parts)-1]] = true
		}
	}
	return suffixes
}

// looksLikeMetricCandidate reports whether tok's final underscore-delimited
// word is one a registered metric name actually ends in. This is what
// excludes `ledger_app` (ends "app") / `ledger_block_mutation` (ends
// "mutation") / `ledger_store` (ends "store") etc. from being flagged as
// mismatched metric names -- they are never candidates in the first place,
// because nothing in the registry ends that way either.
func looksLikeMetricCandidate(tok string, suffixes map[string]bool) bool {
	parts := regexp.MustCompile(`_`).Split(tok, -1)
	if len(parts) == 0 {
		return false
	}
	return suffixes[parts[len(parts)-1]]
}

// docFiles is every doc this test polices for metric-name drift (I-M2 +
// I-M3 + I-N17, merged into one gate per the same finding's own
// recommendation: "被机器盯着的那部分是对的，没盯的 6 个错了 4 个" -- widen
// what's machine-checked instead of hand-auditing each doc separately).
var docFiles = []string{
	"../README.md",
	"../docs/RUNBOOK.md",
	"../docs/CAPACITY.md",
	"../docs/DR.md",
}

// TestDocMetricNamesExistInRegistry pins I-M2/I-M3/I-N17: every ledger_*
// token appearing in README/RUNBOOK/CAPACITY/DR must be a name
// NewPrometheusMetrics() actually registers. Before this fix, README alone
// named six metrics and four of them did not exist under those names
// (wrong word order / wrong suffix); RUNBOOK named three alert-rule-shaped
// identifiers (LedgerRollupBacklog, LedgerCheckpointAgeHigh,
// LedgerEventDeliveryDead) that were never metric names in the first place
// and do not exist anywhere in this repository.
func TestDocMetricNamesExistInRegistry(t *testing.T) {
	registered := registeredMetricNames(t)
	suffixes := metricSuffixesFromRegistry(registered)

	for _, path := range docFiles {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		seen := map[string]bool{}
		for _, tok := range ledgerMetricTokenPattern.FindAllString(string(content), -1) {
			seen[tok] = true
		}
		for tok := range seen {
			if !looksLikeMetricCandidate(tok, suffixes) {
				continue // not metric-shaped: a DB role, trigger name, Go filename, etc.
			}
			if !registered[tok] {
				t.Errorf("%s references %q, which is not a metric NewPrometheusMetrics() registers", filepath.Base(path), tok)
			}
		}
	}
}
