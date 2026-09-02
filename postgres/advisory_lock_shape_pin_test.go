package postgres_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// blockingSessionLock matches a session-level (not _xact_) advisory-lock
// acquisition that BLOCKS (not the _try_ variant), in either the exclusive or
// the shared form.
var blockingSessionLock = regexp.MustCompile(`\bpg_advisory_lock(_shared)?\s*\(`)

// TestNoBlockingSessionAdvisoryLocks is the pin for B-m3: a comment cannot be
// tested, so the reasoning the comment depends on is tested instead.
//
// The residual-risk note on AcquireBalanceLock
// (postgres/sql/queries/journals.sql) argues that a hash collision between
// the bal:/idem: key space and the job-name / migration keys cannot deadlock.
// Its load-bearing argument is that PostgreSQL scopes advisory locks per
// database, and acquireClusterLock deliberately connects to the cluster's
// maintenance database. Its secondary argument — the one the note used to
// state as its ONLY argument, incorrectly, because migrate.go was a
// counter-example at the time — is that nothing in this repository takes a
// blocking session-level advisory lock, so nothing here can sit in a
// wait-for cycle at all.
//
// This test keeps the secondary argument true. Adding a blocking
// session-level pg_advisory_lock anywhere turns it red and forces whoever
// added it back to that comment to re-derive whether their key can close a
// cycle with the balance/idempotency space. Transaction-scoped locks
// (pg_advisory_xact_lock, including the shared period barrier) and the
// non-blocking try_ variants are unaffected: xact locks are what the write
// paths use by design, and a try_ request never enters the wait queue.
func TestNoBlockingSessionAdvisoryLocks(t *testing.T) {
	root := repoRootForAdvisoryScan(t)

	var hits []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "web", "docs", "sqlcgen":
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if ext != ".go" && ext != ".sql" {
			return nil
		}
		if allowedBlockingLockFile(filepath.Base(path)) {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// Skip comments: the argument is about executed SQL, and both
			// journals.sql's note and migrate.go's doc name the pattern in
			// prose on purpose.
			if strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
				continue
			}
			if strings.Contains(line, "pg_try_advisory_lock") {
				continue
			}
			if blockingSessionLock.MatchString(line) {
				rel, _ := filepath.Rel(root, path)
				hits = append(hits, rel+":"+strconv.Itoa(i+1)+": "+trimmed)
			}
		}
		return nil
	}))

	assert.Empty(t, hits,
		"a blocking session-level pg_advisory_lock reappeared. It is the one advisory shape that can hold a lock across "+
			"statements AND sit in PostgreSQL's wait queue, which is what the residual-risk note on AcquireBalanceLock "+
			"(postgres/sql/queries/journals.sql) assumes away. Either use pg_advisory_xact_lock / pg_try_advisory_lock, "+
			"or go re-derive that note for your key. Hits: %v", hits)
}

// allowedBlockingLockFile is the explicit, reasoned allowlist. Both entries
// are test files that must SIMULATE the forbidden shape from outside the
// library -- the thing whose absence from production code this test asserts.
// Kept as a closed list rather than "skip all _test.go" so a blocking
// session lock cannot slip into a test helper that production code then
// starts calling.
func allowedBlockingLockFile(name string) bool {
	switch name {
	case "advisory_lock_shape_pin_test.go":
		// Names the pattern it forbids, in the regex and in the message.
		return true
	case "migrate_cluster_lock_pin_test.go":
		// Stands in for a foreign process holding the cluster migration lock
		// -- there is no other way to hold a session-level lock across the
		// Migrate call under test.
		return true
	default:
		return false
	}
}

func repoRootForAdvisoryScan(t *testing.T) string {
	t.Helper()
	// This test lives in <root>/postgres.
	wd, err := os.Getwd()
	require.NoError(t, err)
	return filepath.Dir(wd)
}
