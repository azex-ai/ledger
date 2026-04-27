// Package slogadapter exposes a tiny adapter that turns *slog.Logger into a
// core.Logger so library users don't have to copy-paste the same wrapper.
package slogadapter

import (
	"log/slog"

	"github.com/azex-ai/ledger/core"
)

// New returns a core.Logger backed by the given *slog.Logger. If l is nil it
// falls back to slog.Default(), so callers always get a working logger.
func New(l *slog.Logger) core.Logger {
	if l == nil {
		l = slog.Default()
	}
	return &slogLogger{l: l}
}

type slogLogger struct {
	l *slog.Logger
}

func (s *slogLogger) Info(msg string, args ...any)  { s.l.Info(msg, args...) }
func (s *slogLogger) Warn(msg string, args ...any)  { s.l.Warn(msg, args...) }
func (s *slogLogger) Error(msg string, args ...any) { s.l.Error(msg, args...) }
