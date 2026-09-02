package delivery

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

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
//
// A panicking handler is recovered and converted into an error like any
// other (I-M9): this is a caller-supplied function, not library code, and
// the same bug in a webhook handler would only ever surface as an HTTP 500
// -- a panic here must not take down the whole process (or, worse under
// Worker.runLoop's own recover, silently skip the rest of this batch's
// handlers and events).
func (d *CallbackDeliverer) Deliver(ctx context.Context, event core.Event) error {
	var errs []error
	for i, h := range d.handlers {
		if err := d.invoke(ctx, i, h, event); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (d *CallbackDeliverer) invoke(ctx context.Context, i int, h func(context.Context, core.Event) error, event core.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("delivery: callback: handler[%d]: panicked: %v\n%s", i, r, debug.Stack())
		}
	}()
	if invokeErr := h(ctx, event); invokeErr != nil {
		return fmt.Errorf("delivery: callback: handler[%d]: %w", i, invokeErr)
	}
	return nil
}
