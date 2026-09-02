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

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type ghWorkflow struct {
	Jobs map[string]struct {
		Uses  string `yaml:"uses"`
		Steps []struct {
			Run              string `yaml:"run"`
			WorkingDirectory string `yaml:"working-directory"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
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

// customBuildTagDecl matches a non-standard build constraint at the top of
// a Go file, e.g. `//go:build e2e`. Deliberately narrow: only single bare
// identifiers, which is the shape this repo currently uses (e2e). A
// compound expression (`e2e && linux`) would need a smarter parser; there
// isn't one in this repo today; add one if that changes rather than
// pretending this regexp already handles it.
var customBuildTagDecl = regexp.MustCompile(`^//go:build ([A-Za-z_][A-Za-z0-9_]*)$`)

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
		firstLine := strings.SplitN(string(src), "\n", 2)[0]
		if m := customBuildTagDecl.FindStringSubmatch(firstLine); m != nil {
			tagLocations[m[1]] = append(tagLocations[m[1]], path)
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
