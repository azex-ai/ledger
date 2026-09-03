package core_test

// F-9 (2026-09-03 independent review): the integration fixture used to
// t.Skip when the Docker daemon was unreachable, so `make test` on a
// machine with Docker stopped printed `ok` for the postgres package
// without running any of its 100+ integration tests. Not-run read as
// pass. working-agreements.md §3 calls that the highest-severity shape of
// bug there is, and the Makefile already carries a long comment governing
// the same failure mode for cached results (-count=1).
//
// The fix lives in internal/postgrestest and the Makefile. This file is
// the pin on the fix: it fails if either half is reverted. It is a
// source-level assertion because the condition it guards -- "Docker is
// absent" -- cannot be produced from inside a test process that needs
// Docker to run.

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const postgrestestPath = "../internal/postgrestest/postgrestest.go"

// skipCall matches a t.Skip / t.Skipf / t.SkipNow call.
var skipCall = regexp.MustCompile(`\bt\.Skip(f|Now)?\(`)

func TestPostgresFixtureFailsRatherThanSkipsWhenDockerIsAbsent(t *testing.T) {
	raw, err := os.ReadFile(postgrestestPath)
	require.NoError(t, err, "read the postgres test fixture")
	src := string(raw)

	// The only skip the fixture may perform is the one the caller asked
	// for on the command line. Everything else -- a daemon that will not
	// answer, an image that will not pull, a container that will not
	// become ready -- is an environment failure, and an environment
	// failure that reads as a pass is worse than a crash.
	for _, m := range skipCall.FindAllStringIndex(src, -1) {
		line := src[strings.LastIndex(src[:m[0]], "\n")+1 : m[1]]
		window := src[max(0, m[0]-400):m[0]]
		assert.Containsf(t, window, "testing.Short()",
			"internal/postgrestest skips outside the -short branch (%q). The only two ways to run this suite without a "+
				"container are -short and DATABASE_URL, and both are things the caller typed. A skip on any other condition "+
				"turns `did not run` into `ok` for every integration test in the repository (F-9)", strings.TrimSpace(line))
	}

	assert.Containsf(t, src, "require.NoErrorf(t, sharedServer.err",
		"internal/postgrestest no longer fails the test when the shared container could not start. "+
			"That error is the one that used to be swallowed as a skip (F-9); it must reach the test as a failure")
}

func TestMakeTestRefusesToRunWithoutADatabase(t *testing.T) {
	raw, err := os.ReadFile("../Makefile")
	require.NoError(t, err, "read the Makefile")

	target := makeTarget(t, string(raw), "test")

	assert.Containsf(t, target, "docker info",
		"the `test` target no longer probes for a reachable Docker daemon. Without it, running `make test` with Docker "+
			"stopped starts a 15-minute run that fails deep inside the postgres package instead of saying so in one line "+
			"before it begins (F-9). The escape hatches are DATABASE_URL and `make test-short`; neither is silent")
	assert.Containsf(t, target, "DATABASE_URL",
		"the `test` target's Docker probe must stand down when DATABASE_URL is set -- that is how CI (and anyone with a "+
			"local server) runs these tests, and a probe that fires there would be a false alarm, which is how probes get deleted")
	assert.Containsf(t, target, "test-short",
		"the `test` target's failure message must name the supported way to run without a database (`make test-short`), "+
			"or the next person to hit it will reintroduce the skip")
}

// makeTarget returns the recipe body of a make target: the lines from
// `name:` up to the next line that starts in column zero.
func makeTarget(t *testing.T, makefile, name string) string {
	t.Helper()
	lines := strings.Split(makefile, "\n")
	start := -1
	for i, line := range lines {
		if strings.HasPrefix(line, name+":") {
			start = i + 1
			break
		}
	}
	require.GreaterOrEqualf(t, start, 0, "the Makefile has no %q target", name)
	var body []string
	for _, line := range lines[start:] {
		if line != "" && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(line, " ") {
			break
		}
		body = append(body, line)
	}
	return strings.Join(body, "\n")
}
