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
