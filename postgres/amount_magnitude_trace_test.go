package postgres_test

import (
	"context"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	otelglobal "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres"
)

func TestStores_AmountMagnitudeBeforeTraceFormatting(t *testing.T) {
	// Tracing configuration is process-wide; do not run these cases in
	// parallel. Restore the provider and the default policy on completion.
	previousProvider := otelglobal.GetTracerProvider()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	otelglobal.SetTracerProvider(provider)
	t.Cleanup(func() {
		otelglobal.SetTracerProvider(previousProvider)
		ledgerotel.SetAttributePolicy(ledgerotel.PolicyMinimal)
		require.NoError(t, provider.Shutdown(context.Background()))
	})

	for _, policy := range []struct {
		name string
		mode ledgerotel.AttributePolicy
	}{
		{"minimal", ledgerotel.PolicyMinimal},
		{"full", ledgerotel.PolicyFull},
	} {
		for _, operation := range []string{"reserve", "create_booking"} {
			t.Run(policy.name+"/"+operation, func(t *testing.T) {
				ledgerotel.SetAttributePolicy(policy.mode)
				pool := postgrestest.SetupDB(t)
				ctx := context.Background()
				ledger := postgres.NewLedgerStore(pool)
				reserver := postgres.NewReserverStore(pool, ledger, postgres.NewVerifiedBalanceStore(pool, nil))
				booker := postgres.NewBookingStore(pool)
				currency := postgrestest.SeedCurrencyWithExponent(t, pool, "USD", "Dollar", 2)
				seedReservableBalance(t, ctx, ledger, pool, 42, currency, decimal.NewFromInt(100))
				classification, err := postgres.NewClassificationStore(pool).CreateClassification(ctx, core.ClassificationInput{
					Code: "magnitude_booking", Name: "Magnitude booking", NormalSide: core.NormalSideCredit, IsSystem: true,
					Lifecycle: &core.Lifecycle{
						Initial: "pending", Terminal: []core.Status{"confirmed"},
						Transitions: map[core.Status][]core.Status{"pending": {"confirmed"}},
					},
				})
				require.NoError(t, err)

				call := func(amount decimal.Decimal, key string) error {
					if operation == "reserve" {
						reservation, err := reserver.Reserve(ctx, core.ReserveInput{
							AccountHolder: 42, CurrencyUID: currency, Amount: amount, IdempotencyKey: key,
						})
						if err == nil {
							require.True(t, reservation.ReservedAmount.Equal(amount))
						} else {
							require.Nil(t, reservation)
						}
						return err
					}
					booking, err := booker.CreateBooking(ctx, core.CreateBookingInput{
						ClassificationCode: classification.Code, AccountHolder: 42, CurrencyUID: currency,
						Amount: amount, IdempotencyKey: key,
					})
					if err == nil {
						require.True(t, booking.Amount.Equal(amount))
					} else {
						require.Nil(t, booking)
					}
					return err
				}
				rows := func() [7]int64 {
					var counts [7]int64
					require.NoError(t, pool.QueryRow(ctx, `SELECT
						(SELECT count(*) FROM reservations), (SELECT count(*) FROM bookings),
						(SELECT count(*) FROM events), (SELECT count(*) FROM journals),
						(SELECT count(*) FROM journal_entries), (SELECT count(*) FROM balance_checkpoints),
						(SELECT count(*) FROM rollup_queue)`).Scan(&counts[0], &counts[1], &counts[2], &counts[3], &counts[4], &counts[5], &counts[6]))
					return counts
				}
				before := rows()
				spanName := "ledger.reserver.reserve"
				if operation == "create_booking" {
					spanName = "ledger.booking.create_booking"
				}

				// Finite values only: the regression is observable through the
				// full-policy trace without materializing a pathological decimal.
				for _, invalid := range []struct {
					name   string
					amount decimal.Decimal
				}{
					{"integer_width", decimal.New(1, 13)},
					{"fractional_width", decimal.New(1, -19)},
					{"non_positive", decimal.NewFromInt(-1)},
				} {
					exporter.Reset()
					err := call(invalid.amount, invalid.name)
					require.ErrorIs(t, err, core.ErrInvalidInput)
					require.Equal(t, before, rows(), "invalid %s must not write rows", invalid.name)
					spans := exporter.GetSpans()
					require.Len(t, spans, 1, "invalid input still emits an ended failure span")
					require.Equal(t, spanName, spans[0].Name)
					require.Equal(t, codes.Error, spans[0].Status.Code)
					require.NotEmpty(t, spans[0].Events, "validation failure remains observable")
					for _, attr := range spans[0].Attributes {
						require.NotEqual(t, "amount", string(attr.Key), "invalid amounts must not be rendered as attributes, including under PolicyFull")
					}
				}
				held, err := reserver.HeldAmount(ctx, 42, currency)
				require.NoError(t, err)
				require.True(t, held.IsZero(), "invalid reservations must not hold funds")

				// A valid request keeps both its monetary effect and the tracing
				// privacy policy. Direct Span.SetAttributes would violate minimal.
				exporter.Reset()
				require.NoError(t, call(decimal.RequireFromString("2.50"), "valid"))
				spans := exporter.GetSpans()
				require.Len(t, spans, 1)
				require.Equal(t, spanName, spans[0].Name)
				require.NotEqual(t, codes.Error, spans[0].Status.Code)
				attributes := make(map[string]string)
				for _, attr := range spans[0].Attributes {
					attributes[string(attr.Key)] = attr.Value.AsString()
				}
				if policy.mode == ledgerotel.PolicyFull {
					require.Equal(t, "2.5", attributes["amount"])
				} else {
					require.NotContains(t, attributes, "amount")
					require.NotContains(t, attributes, "account_holder")
					require.NotContains(t, attributes, "idempotency_key")
				}
				if operation == "reserve" {
					held, err = reserver.HeldAmount(ctx, 42, currency)
					require.NoError(t, err)
					require.True(t, held.Equal(decimal.RequireFromString("2.50")))
				}
			})
		}
	}
}
