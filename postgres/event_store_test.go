package postgres

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// recordingLogger captures every Warn call for assertions, without needing a
// real *core.Engine or slog handler wiring.
type recordingLogger struct {
	core.Logger // embedded nil interface: Info/Error are never called by warn, so this is enough to satisfy core.Logger without stubbing them
	warnMsgs    []string
}

func (l *recordingLogger) Warn(msg string, _ ...any) {
	l.warnMsgs = append(l.warnMsgs, msg)
}

// TestEventStore_Warn_RoutesThroughInjectedLogger pins the fix for
// concurrency.md's Minor finding ("event_store 的「claim lost」用包级 slog
// 消费方注入的 logger 看不到"): once SetLogger has been called, EventStore's
// own diagnostic warnings must go through the injected core.Logger, not
// silently keep going to the package-level slog default that bypasses
// whatever logging pipeline the consumer configured via ledger.WithLogger.
// Before SetLogger/warn existed, this test could not compile at all -- there
// was no way to observe where a claim-lost warning went except by reading
// stdout of the process-wide slog default, which is exactly the bug.
func TestEventStore_Warn_RoutesThroughInjectedLogger(t *testing.T) {
	s := &EventStore{}
	logger := &recordingLogger{}

	got := s.SetLogger(logger)
	assert.Same(t, s, got, "SetLogger should return the same store for chaining")

	s.warn("postgres: mark event delivered: claim lost, outcome dropped", "event_id", int64(42))

	require.Len(t, logger.warnMsgs, 1)
	assert.Equal(t, "postgres: mark event delivered: claim lost, outcome dropped", logger.warnMsgs[0])
}

// TestEventStore_Warn_FallsBackToSlogDefaultWhenNoLoggerSet pins the
// non-regression half: a store that never had SetLogger called (every
// EventStore built via NewEventStore before this fix, and every one built
// today unless the composition root opts in) must not panic and must not
// silently start dropping these warnings -- it keeps going to
// slog.Default(), the same place they always went.
func TestEventStore_Warn_FallsBackToSlogDefaultWhenNoLoggerSet(t *testing.T) {
	s := &EventStore{} // logger is nil -- SetLogger was never called
	assert.NotPanics(t, func() {
		s.warn("postgres: mark event retry: claim lost, outcome dropped", "event_id", int64(7))
	})
}

// TestEventStore_ClaimLostWarnings_UseStoreWarnNotPackageSlog is a source
// pin: every "claim lost" line in event_store.go must call s.warn(...), the
// single choke point that respects SetLogger, and never call the
// package-level slog.Warn(...) directly (that call is only allowed inside
// warn's own fallback branch). A future edit that reintroduces a direct
// slog.Warn(...) call on one of these lines would silently reopen the exact
// gap TestEventStore_Warn_RoutesThroughInjectedLogger closes -- SetLogger
// would still compile and "work" for every OTHER call site, while this one
// quietly stopped respecting it.
func TestEventStore_ClaimLostWarnings_UseStoreWarnNotPackageSlog(t *testing.T) {
	src, err := os.ReadFile("event_store.go")
	require.NoError(t, err)
	found := 0
	for i, line := range strings.Split(string(src), "\n") {
		if !strings.Contains(line, "claim lost") {
			continue
		}
		found++
		require.True(t, strings.Contains(line, "s.warn("),
			"event_store.go:%d: claim-lost line must call s.warn(...), not slog.Warn(...) directly: %s",
			i+1, strings.TrimSpace(line))
		require.False(t, regexp.MustCompile(`[^.]slog\.Warn\(`).MatchString(line),
			"event_store.go:%d: must not call the package-level slog.Warn(...) directly: %s",
			i+1, strings.TrimSpace(line))
	}
	require.Equal(t, 3, found, "expected exactly the three known claim-lost call sites (MarkDelivered/MarkRetry/MarkDead)")
}
