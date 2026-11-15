package core_test

// F-9 (2026-09-03 independent review): the integration fixture used to
// t.Skip when its database was unavailable, so `make test` printed `ok` for
// the postgres package without running any of its 100+ integration tests.
// Not-run read as pass. working-agreements.md §3 calls that the
// highest-severity shape of bug there is, and the Makefile already carries a
// long comment governing the same failure mode for cached results
// (-count=1).
//
// 2026-11-15: the suite no longer uses Docker or testcontainers at all. It
// runs against one PostgreSQL server -- DATABASE_URL or the shared local
// instance -- and an unreachable server fails the test. This file pins both
// halves: the fixture fails rather than skips, and `make test` neither
// probes for nor depends on Docker. It is a source-level assertion because
// "the server is absent" cannot be produced from inside a test process that
// needs the server to run.

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

func TestPostgresFixtureFailsRatherThanSkipsWhenTheServerIsAbsent(t *testing.T) {
	raw, err := os.ReadFile(postgrestestPath)
	require.NoError(t, err, "read the postgres test fixture")
	src := string(raw)

	// The only skip the fixture may perform is the one the caller asked
	// for on the command line. Everything else -- a server that will not
	// answer, bad credentials -- is an environment failure, and an
	// environment failure that reads as a pass is worse than a crash.
	for _, m := range skipCall.FindAllStringIndex(src, -1) {
		line := src[strings.LastIndex(src[:m[0]], "\n")+1 : m[1]]
		window := src[max(0, m[0]-400):m[0]]
		assert.Containsf(t, window, "testing.Short()",
			"internal/postgrestest skips outside the -short branch (%q). The only way to run this suite without a "+
				"database is -short, which the caller typed. A skip on any other condition turns `did not run` into `ok` "+
				"for every integration test in the repository (F-9)", strings.TrimSpace(line))
	}

	assert.Containsf(t, src, "require.NoErrorf(t, admin.Ping(ctx)",
		"internal/postgrestest no longer fails the test when the test server cannot be reached. "+
			"That error is the one that used to be swallowed as a skip (F-9); it must reach the test as a failure")
	for _, forbidden := range []string{"testcontainers", "docker"} {
		assert.NotContainsf(t, strings.ToLower(src), forbidden,
			"internal/postgrestest must not depend on %s: the suite runs against one PostgreSQL server (docs/TESTING.md)", forbidden)
	}
}

func TestMakeTestRunsAgainstOneServerWithoutDocker(t *testing.T) {
	raw, err := os.ReadFile("../Makefile")
	require.NoError(t, err, "read the Makefile")

	target := makeTarget(t, string(raw), "test")

	assert.NotContainsf(t, strings.ToLower(target), "docker",
		"the `test` target must not probe for or depend on Docker: the suite runs against one PostgreSQL server "+
			"(DATABASE_URL or the shared local instance), and a missing Docker daemon must neither fail nor skip it")
	assert.Containsf(t, target, "go test -race -timeout 15m -count=1 ./...",
		"the `test` target must run the uncached race suite")
	assert.Containsf(t, target, "pgtestclean",
		"the `test` target must remove what the run left on the shared server (internal/pgtestenv/pgtestclean)")
	assert.Containsf(t, target, "exit $$status",
		"the `test` target must exit with go test's status, not the cleanup's alone")
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
