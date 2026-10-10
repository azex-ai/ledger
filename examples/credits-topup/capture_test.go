package main

import (
	"maps"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

func TestUsage_CaptureMetadataAndStableKeys(t *testing.T) {
	for _, mode := range []struct {
		name    string
		partial bool
		held    string
	}{{"full", false, "0"}, {"partial", true, "25"}} {
		t.Run(mode.name, func(t *testing.T) {
			svc, admin, usdc, credits := fixture(t)
			ctx := t.Context()
			deposit(t, svc, usdc)
			require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
			rsv := reserve(t, svc, credits, "quoted-job", 50)
			currency, err := svc.Currencies().GetCurrency(ctx, credits)
			require.NoError(t, err)
			usage, err := testPricing(t).price(ctx, *currency, usageQuantity{"IMAGE", decimal.NewFromInt(1)})
			require.NoError(t, err)
			metadata, err := usage.metadata()
			require.NoError(t, err)
			metadata["host_reference"] = "image-provider-result"
			original := maps.Clone(metadata)
			for range 2 {
				require.NoError(t, captureUsage(ctx, svc, rsv, usage.amount, "usage-event", mode.partial, metadata))
			}
			require.Equal(t, original, metadata, "the host's quote snapshot is not mutated")
			var uid string
			require.NoError(t, admin.QueryRow(ctx, "SELECT uid FROM journals WHERE idempotency_key=$1", "usage-event:charge").Scan(&uid))
			journal, _, err := svc.Queries().GetJournal(ctx, uid)
			require.NoError(t, err)
			require.Equal(t, rsv.UID, journal.Metadata["reservation_uid"])
			require.Equal(t, mode.name, journal.Metadata["capture_mode"])
			require.Equal(t, "credits_spend", journal.Metadata["capture_template_code"])
			require.Equal(t, "usage-event", journal.Metadata["usage_event_id"])
			for key, value := range original {
				require.Equal(t, value, journal.Metadata[key])
			}
			require.Equal(t, core.AuthStatusUnsignedTxMode, journal.AuthStatus)
			require.Equal(t, 4, journalCount(t, admin))
			balance(t, svc, credits, "975", mode.held)
		})
	}
}

func TestUsage_ZeroCostReleasesWithoutCharge(t *testing.T) {
	svc, admin, usdc, credits := fixture(t)
	ctx := t.Context()
	deposit(t, svc, usdc)
	require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
	rsv := reserve(t, svc, credits, "free-job", 50)
	require.ErrorIs(t, captureCredits(ctx, svc, rsv, decimal.Zero, "free-result", true), core.ErrInvalidInput)
	balance(t, svc, credits, "1000", "50")
	for range 2 {
		require.NoError(t, captureCredits(ctx, svc, rsv, decimal.Zero, "free-result", false))
	}
	balance(t, svc, credits, "1000", "0")
	require.Equal(t, 3, journalCount(t, admin))
	stored, err := svc.ReservationReader().GetReservation(ctx, rsv.UID)
	require.NoError(t, err)
	require.Equal(t, core.ReservationStatusReleased, stored.Status)
	var receipts int
	require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM reservation_operation_receipts WHERE idempotency_key=$1", "free-result:release").Scan(&receipts))
	require.Equal(t, 1, receipts)
}

func TestCreditsScenario_RejectsLegacyDemoBeforeWrites(t *testing.T) {
	for _, legacy := range []string{"journal", "reservation"} {
		t.Run(legacy, func(t *testing.T) {
			svc, admin, usdc, credits := fixture(t)
			ctx := t.Context()
			wantUSDC, wantCredits, wantHeld := "1", "0", "0"
			if legacy == "journal" {
				_, err := svc.JournalWriter().ExecuteTemplate(ctx, "deposit_confirm", core.TemplateParams{
					HolderID: userID, CurrencyUID: usdc, IdempotencyKey: "credits-demo-v2:deposit",
					Amounts: map[string]decimal.Decimal{"amount": decimal.NewFromInt(1)},
				})
				require.NoError(t, err)
			} else {
				deposit(t, svc, usdc)
				require.NoError(t, purchaseCredits(ctx, svc, usdc, credits, decimal.NewFromInt(1), "purchase"))
				reserve(t, svc, credits, "credits-demo-v2:stream:reserve", 50)
				wantUSDC, wantCredits, wantHeld = "0", "1000", "50"
			}
			counts := func() (got [6]int) {
				require.NoError(t, admin.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM journals), (SELECT count(*) FROM journal_entries),
					(SELECT count(*) FROM reservations), (SELECT count(*) FROM reservation_operation_receipts),
					(SELECT count(*) FROM reservation_settlement_legs), (SELECT count(*) FROM rollup_queue)
				`).Scan(&got[0], &got[1], &got[2], &got[3], &got[4], &got[5]))
				return
			}
			before := counts()
			// setup and scenario are independently protected entry points. The
			// bootstrap schema migration still precedes them in the executable.
			_, _, err := setup(ctx, svc, testPricing(t))
			require.ErrorIs(t, err, core.ErrConflict)
			require.ErrorContains(t, err, "fresh dedicated example database")
			err = scenario(ctx, svc, usdc, credits, testPricing(t))
			require.ErrorIs(t, err, core.ErrConflict)
			require.Equal(t, before, counts())
			balance(t, svc, usdc, wantUSDC, "0")
			balance(t, svc, credits, wantCredits, wantHeld)
			var v3Events int
			require.NoError(t, admin.QueryRow(ctx, "SELECT count(*) FROM journals WHERE idempotency_key LIKE $1", demoNamespace+":%").Scan(&v3Events))
			require.Zero(t, v3Events, "new fixture identity cannot double-fund an old demo database")
		})
	}
}
