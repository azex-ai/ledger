package delivery

import (
	"context"
	"fmt"

	"github.com/azex-ai/ledger/core"
)

// CallbackDeliverer delivers events via synchronous function callbacks.
// Used in library mode where the caller registers handlers directly.
type CallbackDeliverer struct {
	handlers []func(context.Context, core.Event) error
}

// NewCallbackDeliverer creates a new CallbackDeliverer.
func NewCallbackDeliverer() *CallbackDeliverer {
	return &CallbackDeliverer{}
}

// OnEvent registers a callback handler for events.
func (d *CallbackDeliverer) OnEvent(fn func(context.Context, core.Event) error) {
	d.handlers = append(d.handlers, fn)
}

// Deliver calls all registered handlers synchronously.
func (d *CallbackDeliverer) Deliver(ctx context.Context, event core.Event) error {
	for _, h := range d.handlers {
		if err := h(ctx, event); err != nil {
			return fmt.Errorf("delivery: callback: %w", err)
		}
	}
	return nil
}
