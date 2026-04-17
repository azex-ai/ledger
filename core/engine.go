package core

// Engine is the central ledger engine holding all dependencies.
type Engine struct {
	logger  Logger
	metrics Metrics
}

// Option configures the Engine.
type Option func(*Engine)

func WithLogger(l Logger) Option {
	return func(e *Engine) { e.logger = l }
}

func WithMetrics(m Metrics) Option {
	return func(e *Engine) { e.metrics = m }
}

func NewEngine(opts ...Option) *Engine {
	e := &Engine{
		logger:  NopLogger(),
		metrics: NopMetrics(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func (e *Engine) Logger() Logger   { return e.logger }
func (e *Engine) Metrics() Metrics { return e.metrics }
