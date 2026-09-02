package core

// Logger is the observability interface for structured logging.
// Inject slog, zap, zerolog, or any implementation. Default: nopLogger (silent).
type Logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

type nopLogger struct{}

func (nopLogger) Info(string, ...any)  {}
func (nopLogger) Warn(string, ...any)  {}
func (nopLogger) Error(string, ...any) {}

// NopLogger returns a no-op logger.
func NopLogger() Logger { return nopLogger{} }

// IsNopLogger reports whether l is the library's own silent default
// (NopLogger). It exists so a component whose only failure signal is a log
// line can refuse to start silently rather than write that line into
// /dev/null: see service.Worker.Run, which returns an error instead of
// booting when its logger is this one and the consumer has not explicitly
// opted into a silent worker.
//
// It deliberately recognises only this package's own no-op implementation.
// A consumer who injects their own discarding logger has made that choice
// visibly, at their composition root; the failure this guards against is
// the one nobody chose — the default.
func IsNopLogger(l Logger) bool {
	_, ok := l.(nopLogger)
	return ok
}
