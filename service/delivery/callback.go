package delivery

import (
	"context"
	"errors"
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

// Deliver calls all registered handlers synchronously. Every handler sees
// the event even when an earlier one fails — a buggy subscriber must not
// starve its healthy neighbours of the event stream. All handler errors are
// joined into the returned error.
func (d *CallbackDeliverer) Deliver(ctx context.Context, event core.Event) error {
	var errs []error
	for i, h := range d.handlers {
		if err := h(ctx, event); err != nil {
			errs = append(errs, fmt.Errorf("delivery: callback: handler[%d]: %w", i, err))
		}
	}
	return errors.Join(errs...)
}
