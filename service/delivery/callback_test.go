package delivery

import (
	"context"
	"errors"
	"testing"

	"github.com/azex-ai/ledger/core"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallbackDeliverer_Deliver(t *testing.T) {
	ctx := context.Background()
	event := core.Event{
		ClassificationCode: "deposit",
		OperationID:        1,
		FromStatus:         "pending",
		ToStatus:           "confirmed",
	}

	t.Run("no handlers", func(t *testing.T) {
		d := NewCallbackDeliverer()
		assert.NoError(t, d.Deliver(ctx, event))
	})

	t.Run("one handler called with event", func(t *testing.T) {
		d := NewCallbackDeliverer()
		var received core.Event
		d.OnEvent(func(_ context.Context, e core.Event) error {
			received = e
			return nil
		})

		err := d.Deliver(ctx, event)
		require.NoError(t, err)
		assert.Equal(t, event.OperationID, received.OperationID)
		assert.Equal(t, event.ToStatus, received.ToStatus)
	})

	t.Run("handler returns error", func(t *testing.T) {
		d := NewCallbackDeliverer()
		handlerErr := errors.New("handler failed")
		d.OnEvent(func(_ context.Context, _ core.Event) error {
			return handlerErr
		})

		err := d.Deliver(ctx, event)
		require.Error(t, err)
		assert.ErrorIs(t, err, handlerErr)
	})
}
