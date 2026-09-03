package core_test

// F-M6 / F-m8 / F-M5 (2026-09-02 audit): three "the check exists, nothing
// runs it" shapes at the CI-configuration layer itself, none of which any
// Go test previously touched:
//   - go-release.yml's verify job hand-copied three steps from ci.yml
//     instead of sharing them, and fell behind (missing lint, sqlc-diff,
//     govulncheck, both submodules) without anything noticing.
//   - go.work lists five workspace modules; two (internal/postgrestest,
//     anchors/r2/internal/miniotest) had zero go vet/lint/build coverage in
//     ci.yml, and nothing would have caught a sixth module added the same
//     way.
//   - chains/evm/e2e_test.go's `//go:build e2e` tag was never passed by any
//     command in .github/ or Makefile, so it was never compiled, let alone
//     run, by anything.
//
// These are read-only textual/structural checks on the workflow YAML and
// Makefile -- no `act` or `actionlint` invocation, no Go module graph
// involved. They only prove "the two files still agree" / "every workspace
// module has a CI step" / "every custom build tag is referenced
// somewhere" -- not that the CI actually passes, which is GitHub's job.
//
// M-1 and M6b (W3 adversarial review of the gates) are why the checks below
// look at step CONTENT and at the whole build-constraint prologue:
//
//   - The shared-workflow fix was structural but shallow. Both callers
//     `uses:` the same file, and TestGoWorkModulesAllCoveredByGoVerify
//     accepted any step that merely carried a `working-directory:`. The
//     reviewer replaced the root module's entire `go test -race ./...` step
//     with `echo skipped` and all three CI gates plus the core package
//     stayed green. TestGoVerifyRunsRealCommandsForEveryModule now derives
//     the {vet, build, lint, test} matrix from go.work and the presence of
//     _test.go files, and requires a step whose `run` actually invokes that
//     command in that directory.
//   - The build-tag check read only the FIRST line of each file, so a
//     license header or any comment above `//go:build nightly` hid the tag
//     -- and a comment above the constraint is ordinary Go. It now parses
//     the whole constraint prologue with go/build/constraint, so compound
//     expressions are understood rather than silently unmatched.

import (
	"go/build/constraint"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ghStep struct {
	Run              string `yaml:"run"`
	Uses             string `yaml:"uses"`
	WorkingDirectory string `yaml:"working-directory"`
	With             struct {
		WorkingDirectory string `yaml:"working-directory"`
		FetchDepth       *int   `yaml:"fetch-depth"`
		FetchTags        *bool  `yaml:"fetch-tags"`
	} `yaml:"with"`
}

// dir is the directory a step runs in, whether it says so as a step key
// (`working-directory:`) or as an action input (golangci-lint-action's
// `with: working-directory:`). "" means the repository root.
func (s ghStep) dir() string {
	d := s.WorkingDirectory
	if d == "" {
		d = s.With.WorkingDirectory
	}
	if d == "" {
		return "."
	}
	return strings.TrimSuffix(d, "/")
}

type ghJob struct {
	Uses  string   `yaml:"uses"`
	Steps []ghStep `yaml:"steps"`
}

type ghWorkflow struct {
	Jobs map[string]ghJob `yaml:"jobs"`
}

func loadWorkflow(t *testing.T, path string) ghWorkflow {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var wf ghWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return wf
}

// TestReleaseWorkflowUsesSameVerifyAsGoVerify pins F-M6: go-release.yml's
// root-module verify job and ci.yml's verify job must both `uses:` the same
// reusable workflow file. This is the structural fix, not a policy nobody
// enforces: as long as both callers point at the identical file, they
// cannot re-diverge the way go-release.yml drifted from ci.yml before --
// adding a step to one adds it to both, by construction.
func TestReleaseWorkflowUsesSameVerifyAsGoVerify(t *testing.T) {
	ci := loadWorkflow(t, "../.github/workflows/ci.yml")
	release := loadWorkflow(t, "../.github/workflows/go-release.yml")

	ciVerify, ok := ci.Jobs["verify"]
	if !ok || ciVerify.Uses == "" {
		t.Fatal("ci.yml has no 'verify' job with a 'uses:' -- has it stopped calling the reusable workflow?")
	}
	releaseVerify, ok := release.Jobs["verify"]
	if !ok || releaseVerify.Uses == "" {
		t.Fatal("go-release.yml has no 'verify' job with a 'uses:' -- has it gone back to hand-copied steps?")
	}
	if ciVerify.Uses != releaseVerify.Uses {
		t.Errorf("ci.yml's verify job uses %q but go-release.yml's verify job uses %q -- "+
			"they must call the identical reusable workflow file or they can silently drift apart again",
			ciVerify.Uses, releaseVerify.Uses)
	}
	const want = "./.github/workflows/go-verify.yml"
	if ciVerify.Uses != want {
		t.Errorf("expected both verify jobs to use %q, got %q", want, ciVerify.Uses)
	}
}

// goVerifyPath is the reusable workflow both ci.yml and go-release.yml call;
// its step list is the only definition of what CI actually runs.
const goVerifyPath = "../.github/workflows/go-verify.yml"

// workspaceModules returns the modules go.work lists, root first.
func workspaceModules(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../go.work")
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}
	useBlock := string(raw)
	if i := strings.Index(useBlock, "use ("); i >= 0 {
		useBlock = useBlock[i:]
	}
	modules := []string{"."}
	for _, m := range goWorkUseLine.FindAllStringSubmatch(useBlock, -1) {
		modules = append(modules, m[1])
	}
	if len(modules) == 1 {
		t.Fatal("no non-root modules found in go.work's use() block -- the regexp or go.work's format changed")
	}
	sort.Strings(modules)
	return modules
}

// moduleHasTests reports whether a module directory contains any _test.go
// file, so "must run go test" is derived from the tree rather than from a
// list somebody keeps in sync. internal/postgrestest and
// anchors/r2/internal/miniotest have none -- they are fixtures -- and are
// correctly not required to have a test step.
func moduleHasTests(t *testing.T, module string) bool {
	t.Helper()
	root := filepath.Join("..", module)
	found := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web", "testdata":
				return filepath.SkipDir
			}
			// Do not descend into a nested workspace module: its tests are
			// its own module's obligation.
			if path != root {
				if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), "_test.go") {
			found = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s for test files: %v", root, err)
	}
	return found
}

// runInvokes reports whether a step's `run` script actually invokes cmd
// (e.g. "go test") over the WHOLE module -- the command at the start of a
// line, with `./...` as its package pattern. Both halves matter:
//
//   - at the start of a line, so `echo skipped` and a comment mentioning
//     the command do not count;
//   - over `./...`, so a narrower invocation does not stand in for the
//     module-wide one. The three `-fuzz` steps run `go test ./core/`, which
//     is why replacing the root module's `go test -race ./...` step with
//     `echo skipped` still looked covered on the first attempt at this gate.
func runInvokes(run, cmd string) bool {
	for _, line := range strings.Split(run, "\n") {
		line = strings.TrimSpace(line)
		var invoked bool
		for _, prefix := range []string{"", "env ", "sudo "} {
			if strings.HasPrefix(line, prefix+cmd+" ") {
				invoked = true
				break
			}
		}
		if invoked && strings.Contains(line, "./...") {
			return true
		}
	}
	return false
}

// TestGoVerifyRunsRealCommandsForEveryModule closes M-1: the previous pair of
// CI gates checked that go-release.yml and ci.yml point at the same file, and
// that each workspace module appears as SOME step's working-directory. Neither
// looked at what those steps run. Deleting the root module's `go test` step
// left every gate green.
//
// The required matrix is derived: {go vet, go build} for every module in
// go.work, golangci-lint for every module, and `go test` for every module
// that has _test.go files. A step satisfies a cell only if its `run` invokes
// that command in that module's directory.
func TestGoVerifyRunsRealCommandsForEveryModule(t *testing.T) {
	verify := loadWorkflow(t, goVerifyPath)

	type coverage struct{ vet, build, test, lint, vulncheck bool }
	covered := map[string]*coverage{}
	get := func(dir string) *coverage {
		if covered[dir] == nil {
			covered[dir] = &coverage{}
		}
		return covered[dir]
	}
	for _, job := range verify.Jobs {
		for _, step := range job.Steps {
			c := get(step.dir())
			if strings.Contains(step.Uses, "golangci-lint-action") {
				c.lint = true
			}
			if step.Run == "" {
				continue
			}
			switch {
			case runInvokes(step.Run, "go vet"):
				c.vet = true
			case runInvokes(step.Run, "go build"):
				c.build = true
			case runInvokes(step.Run, "go test"):
				c.test = true
			}
			// govulncheck is not a `go` subcommand, so it needs its own
			// arm rather than a switch case: the install line and the scan
			// line can live in the same `run:` block, and the switch above
			// would have stopped at the first match.
			if runInvokes(step.Run, "govulncheck") {
				c.vulncheck = true
			}
		}
	}

	for _, module := range workspaceModules(t) {
		c := get(module)
		want := map[string]bool{"go vet": c.vet, "go build": c.build, "golangci-lint": c.lint}
		if moduleHasTests(t, module) {
			want["go test"] = c.test
		}
		// F-8 (2026-09-03 independent review): vulncheck used to be absent
		// from this matrix, so `govulncheck ./...` running in the root and
		// nowhere else was invisible -- anchors/r2 (aws-sdk-go-v2) and
		// chains/evm (go-ethereum) shipped unscanned. Required only for
		// modules a consumer can import: the two `internal/` fixture
		// modules exist to keep testcontainers out of consumer builds
		// (CLAUDE.md Gotchas), and scanning them would report CVEs on code
		// no production path can reach -- the precise thing govulncheck's
		// reachability analysis is for.
		if consumerReachableModule(module) {
			want["govulncheck"] = c.vulncheck
		}
		var missing []string
		for cmd, ok := range want {
			if !ok {
				missing = append(missing, cmd)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("go-verify.yml has no step that actually runs %v for workspace module %q -- "+
				"a step that merely carries the right working-directory (or runs `echo`) is not coverage: "+
				"replacing the root module's whole `go test` step with `echo skipped` used to leave every CI gate green (M-1)",
				missing, module)
		}
	}
}

// TestGoVerifyFetchesHistoryWhereTheBreakingGatesRun closes the other half of
// C-3: the breaking-change gates diff against the last release tag, and
// actions/checkout's default depth-1 clone has no tags. Those gates now fail
// rather than skip when they cannot resolve a baseline, which means the CI
// checkout must fetch history -- and that requirement has to be enforced
// here, or the next person who trims the checkout turns every one of those
// gates red (or, worse, someone "fixes" that by restoring the skip).
func TestGoVerifyFetchesHistoryWhereTheBreakingGatesRun(t *testing.T) {
	verify := loadWorkflow(t, goVerifyPath)

	checked := 0
	for name, job := range verify.Jobs {
		runsRootTests := false
		for _, step := range job.Steps {
			if step.dir() == "." && step.Run != "" && runInvokes(step.Run, "go test") {
				runsRootTests = true
			}
		}
		if !runsRootTests {
			continue
		}
		checked++
		full := false
		for _, step := range job.Steps {
			if !strings.Contains(step.Uses, "actions/checkout") {
				continue
			}
			if step.With.FetchDepth != nil && *step.With.FetchDepth == 0 {
				full = true
			}
			if step.With.FetchTags != nil && *step.With.FetchTags {
				full = true
			}
		}
		if !full {
			t.Errorf("job %q in go-verify.yml runs the root module's tests but checks out without `fetch-depth: 0` "+
				"(or `fetch-tags: true`). TestAPISurface_BreakingChangesAreDocumented and "+
				"TestChangelogListsBreakingGoAPIChanges diff the exported Go API against the last release tag; "+
				"a depth-1 clone has no tags, and both gates fail rather than skip when they cannot resolve one", name)
		}
	}
	if checked == 0 {
		t.Fatal("no job in go-verify.yml runs the root module's tests -- either the workflow regressed or this gate stopped finding it")
	}
}

// goWorkUseLine matches one module path inside go.work's `use (...)` block,
// e.g. "	./anchors/r2/internal/miniotest".
var goWorkUseLine = regexp.MustCompile(`(?m)^\s*\./(\S+)\s*$`)

// TestGoWorkModulesAllCoveredByGoVerify pins F-m8's general shape: every
// module go.work lists (other than the root ".") must have at least one
// `working-directory:` step somewhere in go-verify.yml. Derived from go.work
// itself, not a hand-copied list, so a sixth module added to the workspace
// without a matching CI step goes red here instead of silently shipping
// with zero vet/lint/build/test coverage the way
// internal/postgrestest and anchors/r2/internal/miniotest did.
func TestGoWorkModulesAllCoveredByGoVerify(t *testing.T) {
	raw, err := os.ReadFile("../go.work")
	if err != nil {
		t.Fatalf("read go.work: %v", err)
	}
	useBlock := raw
	if i := strings.Index(string(raw), "use ("); i >= 0 {
		useBlock = raw[i:]
	}

	var modules []string
	for _, m := range goWorkUseLine.FindAllStringSubmatch(string(useBlock), -1) {
		modules = append(modules, m[1])
	}
	if len(modules) == 0 {
		t.Fatal("no non-root modules found in go.work's use() block -- the regexp or go.work's format changed")
	}

	verify := loadWorkflow(t, "../.github/workflows/go-verify.yml")
	release := loadWorkflow(t, "../.github/workflows/go-release.yml")

	coveredDirs := map[string]bool{}
	for _, wf := range []ghWorkflow{verify, release} {
		for _, job := range wf.Jobs {
			for _, step := range job.Steps {
				if step.WorkingDirectory != "" {
					coveredDirs[strings.TrimSuffix(step.WorkingDirectory, "/")] = true
				}
			}
		}
	}

	sort.Strings(modules)
	var uncovered []string
	for _, m := range modules {
		if !coveredDirs[m] {
			uncovered = append(uncovered, m)
		}
	}
	if len(uncovered) > 0 {
		t.Errorf("go.work lists module(s) with no working-directory: step in go-verify.yml or go-release.yml: %v -- "+
			"a workspace module with zero CI coverage fails silently (everything else just mysteriously breaks)", uncovered)
	}
}

// standardBuildTags are the constraint identifiers the toolchain supplies on
// its own -- GOOS, GOARCH, and the handful of well-known feature tags. They
// need no `-tags` reference from CI because no command ever passes them.
// Anything NOT in this set is treated as a custom tag that some workflow or
// the Makefile must name, which is the fail-closed direction: an identifier
// this list has never heard of defaults to "must be referenced".
var standardBuildTags = map[string]bool{
	// GOOS
	"aix": true, "android": true, "darwin": true, "dragonfly": true,
	"freebsd": true, "hurd": true, "illumos": true, "ios": true, "js": true,
	"linux": true, "netbsd": true, "openbsd": true, "plan9": true,
	"solaris": true, "wasip1": true, "windows": true, "zos": true,
	// GOARCH
	"386": true, "amd64": true, "arm": true, "arm64": true, "loong64": true,
	"mips": true, "mips64": true, "mips64le": true, "mipsle": true,
	"ppc64": true, "ppc64le": true, "riscv64": true, "s390x": true,
	"sparc64": true, "wasm": true,
	// Toolchain / feature tags
	"cgo": true, "gc": true, "gccgo": true, "race": true, "msan": true,
	"asan": true, "boringcrypto": true, "purego": true, "unix": true,
	// `//go:build ignore` marks a file no build ever includes.
	"ignore": true,
}

// buildTagsIn returns the custom build-constraint identifiers declared by a
// Go source file.
//
// M6b (W3 adversarial review of the gates): this used to read
// strings.SplitN(src, "\n", 2)[0] -- the FIRST line only -- so putting any
// comment above `//go:build nightly` (a license header, the ordinary Go
// convention) hid the tag from the gate entirely, and the reviewer's
// mutation of exactly that shape passed. The constraint may appear anywhere
// in the prologue: comments and blank lines before the `package` clause. And
// rather than a regexp that only understands a bare identifier, this parses
// the expression with go/build/constraint, so `e2e && !windows` yields
// "e2e" (windows being standard) instead of matching nothing at all.
func buildTagsIn(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			break // prologue is over; a //go:build below here is inert
		}
		if !constraint.IsGoBuild(trimmed) {
			continue
		}
		expr, err := constraint.Parse(trimmed)
		if err != nil {
			// An unparseable constraint is not a pass: report it as a tag so
			// the gate names the file instead of ignoring it.
			out = append(out, "<unparseable: "+trimmed+">")
			continue
		}
		for _, tag := range constraintTags(expr) {
			if !standardBuildTags[tag] && !strings.HasPrefix(tag, "go1.") {
				out = append(out, tag)
			}
		}
	}
	return out
}

// constraintTags walks a parsed build constraint and collects its tag names.
func constraintTags(expr constraint.Expr) []string {
	switch e := expr.(type) {
	case *constraint.TagExpr:
		return []string{e.Tag}
	case *constraint.NotExpr:
		return constraintTags(e.X)
	case *constraint.AndExpr:
		return append(constraintTags(e.X), constraintTags(e.Y)...)
	case *constraint.OrExpr:
		return append(constraintTags(e.X), constraintTags(e.Y)...)
	default:
		return nil
	}
}

// TestCustomBuildTagsAppearInCIOrMakefile pins F-M5's general shape: any
// custom `//go:build <tag>` constraint in the repo must be referenced
// (`-tags <tag>` or `-tags=<tag>`) by at least one workflow YAML or the
// Makefile. chains/evm/e2e_test.go's `e2e` tag sat with zero references
// anywhere for long enough that the file was never even compiled by CI;
// this catches the next tag that ships the same way, not just this one.
func TestCustomBuildTagsAppearInCIOrMakefile(t *testing.T) {
	tagLocations := map[string][]string{} // tag -> files declaring it
	err := filepath.WalkDir("..", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, tag := range buildTagsIn(string(src)) {
			tagLocations[tag] = append(tagLocations[tag], path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo for build tags: %v", err)
	}
	if len(tagLocations) == 0 {
		t.Fatal("no custom //go:build tags found -- if chains/evm/e2e_test.go's tag was ever removed, " +
			"update this test's expectations rather than let it silently stop checking anything")
	}

	var referencedIn strings.Builder
	for _, dir := range []string{"../.github/workflows", ".."} {
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || (!strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") && name != "Makefile") {
				continue
			}
			src, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil {
				continue
			}
			referencedIn.Write(src)
			referencedIn.WriteByte('\n')
		}
	}
	haystack := referencedIn.String()

	var uncovered []string
	for tag, files := range tagLocations {
		if !strings.Contains(haystack, "-tags "+tag) && !strings.Contains(haystack, "-tags="+tag) &&
			!strings.Contains(haystack, "-tags '"+tag+"'") && !strings.Contains(haystack, "-tags \""+tag+"\"") {
			uncovered = append(uncovered, tag+" (declared in "+strings.Join(files, ", ")+")")
		}
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("custom build tag(s) with zero -tags reference in any .github/workflows/*.yml or Makefile: %v -- "+
			"these files are never compiled by anything", uncovered)
	}
}

// consumerReachableModule reports whether a workspace module can end up in
// somebody else's build. Go's `internal/` rule makes that a structural
// property, not a judgement call: no module outside this repository can
// import a package under an `internal/` path segment, so the fixture
// modules (internal/postgrestest, anchors/r2/internal/miniotest) are
// unreachable from any consumer by construction.
func consumerReachableModule(module string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(module), "/") {
		if seg == "internal" {
			return false
		}
	}
	return true
}
