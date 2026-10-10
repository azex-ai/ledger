package postgres_test

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

func TestJournalIdempotencyStoredTimePrecision(t *testing.T) {
	for _, mode := range []string{"pool_signed", "pool_unsigned", "tx_unsigned", "authorized_pool", "authorized_tx"} {
		t.Run(mode, func(t *testing.T) {
			pool := postgrestest.SetupDB(t)
			ctx := t.Context()
			f := setupAuthFixture(t, pool, ctx)
			attestor, verifier := newTestAttestor(t, "precision-test")
			store := postgres.NewLedgerStore(pool)
			if mode != "pool_unsigned" {
				store = store.WithAuth(attestor)
			}
			post := func(in core.JournalInput) (*core.Journal, error) {
				var authorized core.AuthorizedJournal
				if mode == "authorized_pool" || mode == "authorized_tx" {
					var err error
					authorized, err = store.Authorize(ctx, in)
					if err != nil {
						return nil, err
					}
				}
				if mode == "authorized_pool" {
					return store.PostAuthorized(ctx, authorized)
				}
				if mode != "tx_unsigned" && mode != "authorized_tx" {
					return store.PostJournal(ctx, in)
				}
				tx, err := pool.Begin(ctx)
				if err != nil {
					return nil, err
				}
				defer tx.Rollback(ctx)
				var journal *core.Journal
				if mode == "authorized_tx" {
					journal, err = store.WithDB(tx).PostAuthorized(ctx, authorized)
				} else {
					journal, err = store.WithDB(tx).PostJournal(ctx, in)
				}
				if err != nil {
					return nil, err
				}
				return journal, tx.Commit(ctx)
			}
			at := time.Date(2026, 10, 10, 0, 0, 0, 123456789, time.UTC)
			input := f.journalInput(8102, "precision:"+mode, decimal.NewFromInt(20))
			input.EffectiveAt = at
			original, err := post(input)
			require.NoError(t, err)
			require.True(t, at.Truncate(time.Microsecond).Equal(original.EffectiveAt))
			status := core.AuthStatusSigned
			switch mode {
			case "pool_unsigned":
				status = core.AuthStatusUnsignedNoAttestor
			case "tx_unsigned":
				status = core.AuthStatusUnsignedTxMode
			}
			require.Equal(t, status, original.AuthStatus)
			if status == core.AuthStatusSigned {
				digest, signature, key, effectiveAt := fetchAuthColumns(t, pool, ctx, original.UID)
				require.NoError(t, core.VerifyJournalAuth(ctx, verifier, input, effectiveAt, digest, signature, key))
			}
			counts := func() [3]int {
				var c [3]int
				require.NoError(t, pool.QueryRow(ctx, `SELECT
					(SELECT count(*) FROM journals), (SELECT count(*) FROM journal_entries),
					(SELECT count(*) FROM rollup_queue)`).Scan(&c[0], &c[1], &c[2]))
				return c
			}
			before := counts()
			require.Equal(t, [3]int{1, 2, 2}, before)
			for _, tc := range []struct {
				name     string
				at       time.Time
				amount   int64
				conflict bool
			}{
				{"exact_nanoseconds", at, 20, false},
				{"same_microsecond_floor_not_round", at.Add(-500 * time.Nanosecond), 20, false},
				{"stored_precision", at.Truncate(time.Microsecond), 20, false},
				{"same_instant_other_timezone", at.In(time.FixedZone("offset", 8*60*60)), 20, false},
				{"previous_microsecond", at.Add(-time.Microsecond), 20, true},
				{"next_microsecond", at.Add(time.Microsecond), 20, true},
				{"changed_amount", at.Truncate(time.Microsecond), 21, true},
				{"zero_remains_default", time.Time{}, 20, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					replay := input
					replay.EffectiveAt = tc.at
					replay.Entries = append([]core.EntryInput(nil), input.Entries...)
					for i := range replay.Entries {
						replay.Entries[i].Amount = decimal.NewFromInt(tc.amount)
					}
					got, err := post(replay)
					if tc.conflict {
						require.ErrorIs(t, err, core.ErrConflict)
					} else {
						require.NoError(t, err)
						require.Equal(t, original.UID, got.UID)
					}
					require.Equal(t, before, counts())
					balance, err := store.GetBalance(ctx, 8102, f.CurrencyUID, f.MainWalletUID)
					require.NoError(t, err)
					require.True(t, balance.Equal(decimal.NewFromInt(20)))
				})
			}
		})
	}
}
