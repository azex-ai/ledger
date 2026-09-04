package postgres_test

// Pins for N1 (`docs/audits/2026-09-03-independent-review/recheck/install-roles.md`)
// and migration 031.
//
// Migration 030 replaced a caller-writable dedup memo with a schedule
// assumption: the per-entry balance trigger skipped its aggregate whenever
// `journals.xmin` was the current transaction's, on the grounds that a
// journals-level deferred constraint trigger was already queued for that
// journal. `SET CONSTRAINTS ALL IMMEDIATE` -- one statement, no privilege
// required, available to every role, in effect for the rest of the
// transaction -- moves that queued check to the end of the INSERT that created
// the journal, when it has no entries yet. Both triggers ran; neither saw the
// entries; a one-sided 999,999 committed.
//
// The general shape, and what this file exists to keep closed:
//
//	evaluation timing is caller-controlled, so no guard may skip work on
//	the grounds that another trigger will run later.
//
// Every pin migration 030 shipped ran under the default DEFERRED mode, and
// `SET CONSTRAINTS` appeared nowhere in the repository, so the whole class was
// invisible. TestBalanceGuard_RefusedUnderEveryConstraintTiming derives the
// timing modes from `pg_trigger.tgdeferrable` instead of naming them, so a
// future deferrable trigger is enrolled in the battery by existing rather than
// by someone remembering.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/internal/postgrestest"
	"github.com/azex-ai/ledger/postgres"
)

// balanceAttackEnv is what every attack in the battery needs: a superuser pool
// for fixtures, an app pool to attack from, and the resolved dimension ids.
type balanceAttackEnv struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	appPool    *pgxpool.Pool
	store      *postgres.LedgerStore
	deps       invariantsFixture
	currencyID int64
	classID    int64
	journalTyp int64
}

// balanceAttack is one way to end a transaction with a journal that does not
// balance by currency. Each returns the error the database gave, or nil if the
// attack committed -- which is the failure this file is about.
type balanceAttack struct {
	name string
	// needsTemp is true for the two attacks that have to create a pg_temp
	// relation, which migration 030 section 3 revoked. They run with
	// TEMPORARY granted back, so the guard is tested without that layer.
	needsTemp bool
	run       func(t *testing.T, env balanceAttackEnv, conn *pgxpool.Conn, timing string, tag string) error
}

// applyTiming issues the constraint-timing statement, if any, on the
// connection the attack is about to use. It is deliberately the first thing in
// the transaction, exactly as the recheck's PoC has it.
func applyTiming(t *testing.T, ctx context.Context, tx pgx.Tx, timing string) {
	t.Helper()
	if timing == "" {
		return
	}
	_, err := tx.Exec(ctx, timing)
	require.NoErrorf(t, err, "the timing statement itself must be accepted, or the attack is not being attempted: %s", timing)
}

// balanceAttacks is the battery: the two shapes migration 030 closed (C1
// relation shadowing, C2 pre-seeded dedup memo) plus the shape N1 opened
// (adopting an already-committed journal's xmin with a no-op UPDATE).
//
// All three are run under every constraint-timing mode. Before 031 the third
// one committed under IMMEDIATE while being correctly refused under DEFERRED,
// which is the entire finding.
var balanceAttacks = []balanceAttack{
	{
		name:      "C1 pg_temp relation shadowing",
		needsTemp: true,
		run: func(t *testing.T, env balanceAttackEnv, conn *pgxpool.Conn, timing, tag string) error {
			journalID := postBalancedJournal(t, env.store, env.pool, env.ctx, env.deps, 8101, decimal.NewFromInt(100), "n1-c1-"+tag)

			if _, err := conn.Exec(env.ctx, `
				CREATE TEMP TABLE IF NOT EXISTS journal_entries (
					journal_id bigint, currency_id bigint, entry_type text, amount numeric
				)`); err != nil {
				return fmt.Errorf("the shadow relation must be creatable, or the attack is not being attempted: %w", err)
			}

			tx, err := conn.Begin(env.ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(env.ctx) }()
			applyTiming(t, env.ctx, tx, timing)

			if _, err := tx.Exec(env.ctx, `
				INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
				VALUES ($1, $2, $3, $4, 'debit', 999999)`,
				journalID, 8101, env.currencyID, env.classID); err != nil {
				return err
			}
			return tx.Commit(env.ctx)
		},
	},
	{
		name:      "C2 pre-seeded dedup memo",
		needsTemp: true,
		run: func(t *testing.T, env balanceAttackEnv, conn *pgxpool.Conn, timing, tag string) error {
			if _, err := conn.Exec(env.ctx, `
				CREATE TEMP TABLE IF NOT EXISTS ledger_balance_checked (journal_id BIGINT PRIMARY KEY)`); err != nil {
				return fmt.Errorf("the memo table must be creatable, or the attack is not being attempted: %w", err)
			}
			if _, err := conn.Exec(env.ctx, `
				INSERT INTO ledger_balance_checked SELECT generate_series(1, 10000) ON CONFLICT DO NOTHING`); err != nil {
				return err
			}

			tx, err := conn.Begin(env.ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(env.ctx) }()
			applyTiming(t, env.ctx, tx, timing)

			var journalID int64
			if err := tx.QueryRow(env.ctx, `
				INSERT INTO public.journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
				VALUES ($1, $2, 999999, 999999, gen_random_uuid()) RETURNING id`,
				env.journalTyp, postgrestest.UniqueKey("n1-c2-"+tag)).Scan(&journalID); err != nil {
				return err
			}
			if _, err := tx.Exec(env.ctx, `
				INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
				VALUES ($1, $2, $3, $4, 'debit', 999999)`,
				journalID, 8202, env.currencyID, env.classID); err != nil {
				return err
			}
			return tx.Commit(env.ctx)
		},
	},
	{
		name: "N1 adopt an already-committed journal's xmin",
		run: func(t *testing.T, env balanceAttackEnv, conn *pgxpool.Conn, timing, tag string) error {
			// Committed honestly, in its own transaction, balanced.
			journalID := postBalancedJournal(t, env.store, env.pool, env.ctx, env.deps, 8303, decimal.NewFromInt(100), "n1-adopt-"+tag)

			tx, err := conn.Begin(env.ctx)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback(env.ctx) }()
			applyTiming(t, env.ctx, tx, timing)

			// A no-op UPDATE: ledger_journals_block_arbitrary_update compares
			// to_jsonb(OLD) with to_jsonb(NEW) and so permits it, and it
			// refreshes xmin. The recheck used a real event_id backfill; this
			// is the same move with no fixture to build, and it is strictly
			// easier for an attacker.
			if _, err := tx.Exec(env.ctx, `UPDATE public.journals SET source = source WHERE id = $1`, journalID); err != nil {
				return err
			}
			if _, err := tx.Exec(env.ctx, `
				INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
				VALUES ($1, $2, $3, $4, 'debit', 777)`,
				journalID, 8303, env.currencyID, env.classID); err != nil {
				return err
			}
			return tx.Commit(env.ctx)
		},
	},
}

func newBalanceAttackEnv(t *testing.T, password string) balanceAttackEnv {
	t.Helper()
	ctx := context.Background()
	pool := postgrestest.SetupDB(t)
	store, deps := setupInvariantsFixture(t, pool, ctx)
	return balanceAttackEnv{
		ctx:        ctx,
		pool:       pool,
		appPool:    newAppPool(t, pool, password),
		store:      store,
		deps:       deps,
		currencyID: postgrestest.InternalID(t, pool, "currencies", deps.Currency),
		classID:    postgrestest.InternalID(t, pool, "classifications", deps.MainWallet),
		journalTyp: postgrestest.InternalID(t, pool, "journal_types", deps.JournalType),
	}
}

// deferrableTriggerNames returns the distinct names of every non-internal
// DEFERRABLE trigger in the schema -- i.e. every trigger whose firing time a
// caller can move with `SET CONSTRAINTS <name> IMMEDIATE`.
//
// Derived, not listed: the point of N1 is that a deferrable trigger nobody
// thought about is a bypass nobody tests for.
func deferrableTriggerNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT t.tgname
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE NOT t.tgisinternal AND t.tgdeferrable AND n.nspname = 'public'
		ORDER BY 1
	`)
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	require.NotEmpty(t, names,
		"sanity: the balance guard is a DEFERRABLE constraint trigger, so at least one must be found -- an empty list would make this whole gate vacuous")
	return names
}

// TestBalanceGuard_RefusedUnderEveryConstraintTiming is the gate.
//
// Three attacks x (default DEFERRED + `SET CONSTRAINTS ALL IMMEDIATE` + one
// mode per deferrable trigger). Every combination must be refused by the
// balance guard, by name -- not by some unrelated error that happens to look
// like a refusal.
func TestBalanceGuard_RefusedUnderEveryConstraintTiming(t *testing.T) {
	env := newBalanceAttackEnv(t, "n1-timing-battery-not-a-real-secret") //nolint:gosec

	timings := []struct{ label, stmt string }{
		{"default (DEFERRED)", ""},
		{"SET CONSTRAINTS ALL IMMEDIATE", "SET CONSTRAINTS ALL IMMEDIATE"},
	}
	for _, name := range deferrableTriggerNames(t, env.ctx, env.pool) {
		timings = append(timings, struct{ label, stmt string }{
			label: "SET CONSTRAINTS " + name + " IMMEDIATE",
			stmt:  "SET CONSTRAINTS " + pgx.Identifier{name}.Sanitize() + " IMMEDIATE",
		})
	}

	// The two pg_temp attacks need the privilege migration 030 revoked. Handing
	// it back for the whole test keeps each layer independently load-bearing:
	// the guard must refuse these without help from the database ACL.
	withTemporaryGrantedBack(t, env.pool, func() {
		for _, timing := range timings {
			for i, attack := range balanceAttacks {
				timing, attack := timing, attack
				t.Run(timing.label+"/"+attack.name, func(t *testing.T) {
					conn, err := env.appPool.Acquire(env.ctx)
					require.NoError(t, err)
					defer conn.Release()

					err = attack.run(t, env, conn, timing.stmt, fmt.Sprintf("%d-%d", i, len(timing.label)))
					require.Error(t, err,
						"%s committed under %q. `SET CONSTRAINTS` needs no privilege and applies to any DEFERRABLE trigger, so a guard that is only correct in one timing mode is not a guard (N1)",
						attack.name, timing.label)
					assert.Contains(t, err.Error(), "unbalanced entries",
						"the refusal must come from the balance guard: %v", err)
				})
			}
		}
	})
}

// TestBalanceGuard_TheSkipIsWhatMadeTimingMatter is the reverse confirmation:
// put migration 030's xmin skip back and the IMMEDIATE half of the battery
// goes red, while the DEFERRED half stays green.
//
// Without this the battery above proves only that the current code refuses
// these attacks, not that removing the skip is what did it -- and a future
// reader looking at the O(N^2) aggregate has every reason to want the skip
// back.
func TestBalanceGuard_TheSkipIsWhatMadeTimingMatter(t *testing.T) {
	env := newBalanceAttackEnv(t, "n1-reverse-confirm-not-a-real-secret") //nolint:gosec

	// Migration 030's guard, restored verbatim: the helper, the journals-level
	// trigger and the skip.
	_, err := env.pool.Exec(env.ctx, `
		CREATE FUNCTION ledger_assert_journal_balanced(p_journal_id BIGINT) RETURNS void
		LANGUAGE plpgsql SET search_path = public, pg_temp AS $fn$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM public.journal_entries WHERE journal_id = p_journal_id
				GROUP BY currency_id
				HAVING SUM(CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END) <> 0
			) THEN
				RAISE EXCEPTION 'journal % has unbalanced entries by currency', p_journal_id
					USING ERRCODE = '23514', CONSTRAINT = 'chk_journal_currency_balance';
			END IF;
		END $fn$;

		CREATE FUNCTION ledger_check_journal_balance() RETURNS TRIGGER
		LANGUAGE plpgsql SET search_path = public, pg_temp AS $fn$
		BEGIN
			PERFORM public.ledger_assert_journal_balanced(NEW.id);
			RETURN NULL;
		END $fn$;

		GRANT EXECUTE ON FUNCTION ledger_assert_journal_balanced(BIGINT) TO ledger_app;

		CREATE OR REPLACE FUNCTION check_journal_currency_balance() RETURNS TRIGGER
		LANGUAGE plpgsql SET search_path = public, pg_temp AS $fn$
		DECLARE
			target_journal_id BIGINT;
			written_here      BOOLEAN;
		BEGIN
			target_journal_id := COALESCE(NEW.journal_id, OLD.journal_id);
			IF target_journal_id IS NULL THEN RETURN NULL; END IF;
			SELECT j.xmin = pg_catalog.pg_current_xact_id()::xid INTO written_here
			  FROM public.journals j WHERE j.id = target_journal_id;
			IF COALESCE(written_here, FALSE) THEN RETURN NULL; END IF;
			PERFORM public.ledger_assert_journal_balanced(target_journal_id);
			RETURN NULL;
		END $fn$;

		CREATE CONSTRAINT TRIGGER trg_check_journal_balance_on_journal
			AFTER INSERT OR UPDATE ON journals
			DEFERRABLE INITIALLY DEFERRED
			FOR EACH ROW EXECUTE FUNCTION ledger_check_journal_balance();`)
	require.NoError(t, err, "restoring migration 030's guard must work; this test is what says 031 is the thing that closed N1")

	// The two shapes the recheck measured, on the restored guard.
	for _, tc := range []struct {
		attack string
		timing string
	}{
		{"C2 pre-seeded dedup memo", "SET CONSTRAINTS ALL IMMEDIATE"},
		{"N1 adopt an already-committed journal's xmin", "SET CONSTRAINTS ALL IMMEDIATE"},
		{"N1 adopt an already-committed journal's xmin", "SET CONSTRAINTS trg_check_journal_balance_on_journal IMMEDIATE"},
	} {
		tc := tc
		t.Run(tc.attack+" under "+tc.timing, func(t *testing.T) {
			var attack balanceAttack
			for _, a := range balanceAttacks {
				if a.name == tc.attack {
					attack = a
				}
			}
			require.NotEmpty(t, attack.name, "attack %q must exist in the battery", tc.attack)

			run := func() error {
				conn, err := env.appPool.Acquire(env.ctx)
				require.NoError(t, err)
				defer conn.Release()
				return attack.run(t, env, conn, tc.timing, "rev")
			}

			var err error
			if attack.needsTemp {
				withTemporaryGrantedBack(t, env.pool, func() { err = run() })
			} else {
				err = run()
			}
			assert.NoError(t, err,
				"with migration 030's skip restored this attack must SUCCEED -- that is the finding. If it now fails, the skip is no longer what made the timing matter and this test's premise needs rewriting, not deleting")
		})
	}
}

// TestBalanceGuard_ImmediateModeRefusesHonestMultiStatementPosting states the
// cost of 031 out loud so it is not later mistaken for a bug.
//
// The guard is DEFERRABLE INITIALLY DEFERRED because a journal is written one
// entry per statement: after the first entry it genuinely does not balance. With
// the aggregate unconditional, a caller who turns deferral off is opting out
// of composing a journal across statements -- the write fails, no money moves,
// and nothing in this library ever issues `SET CONSTRAINTS`.
//
// The direction is what matters: turning the knob makes honest writes fail,
// never dishonest ones succeed.
func TestBalanceGuard_ImmediateModeRefusesHonestMultiStatementPosting(t *testing.T) {
	env := newBalanceAttackEnv(t, "n1-immediate-honest-not-a-real-secret") //nolint:gosec

	conn, err := env.appPool.Acquire(env.ctx)
	require.NoError(t, err)
	defer conn.Release()

	tx, err := conn.Begin(env.ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(env.ctx) }()

	_, err = tx.Exec(env.ctx, "SET CONSTRAINTS ALL IMMEDIATE")
	require.NoError(t, err)

	var journalID int64
	require.NoError(t, tx.QueryRow(env.ctx, `
		INSERT INTO public.journals (journal_type_id, idempotency_key, total_debit, total_credit, uid)
		VALUES ($1, $2, 100, 100, gen_random_uuid()) RETURNING id`,
		env.journalTyp, postgrestest.UniqueKey("n1-honest-immediate")).Scan(&journalID))

	_, err = tx.Exec(env.ctx, `
		INSERT INTO public.journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount)
		VALUES ($1, $2, $3, $4, 'debit', 100)`, journalID, 8404, env.currencyID, env.classID)
	require.Error(t, err, "under IMMEDIATE the first entry of an honest journal is refused -- fail-closed, and the documented cost of not trusting the schedule")
	assert.Contains(t, err.Error(), "unbalanced entries")
}

// TestBalanceGuard_DeferredModeStillPostsHonestJournals is the control for the
// one above: the default mode -- the only one anything in this library uses --
// is untouched, including a journal composed one statement at a time.
func TestBalanceGuard_DeferredModeStillPostsHonestJournals(t *testing.T) {
	env := newBalanceAttackEnv(t, "n1-deferred-honest-not-a-real-secret") //nolint:gosec

	entries := make([]core.EntryInput, 0, 12)
	for i := 0; i < 6; i++ {
		entries = append(entries,
			core.EntryInput{AccountHolder: 8505, CurrencyUID: env.deps.Currency, ClassificationUID: env.deps.MainWallet, EntryType: core.EntryTypeDebit, Amount: decimal.NewFromInt(11)},
			core.EntryInput{AccountHolder: -8505, CurrencyUID: env.deps.Currency, ClassificationUID: env.deps.Custodial, EntryType: core.EntryTypeCredit, Amount: decimal.NewFromInt(11)},
		)
	}
	_, err := env.store.PostJournal(env.ctx, core.JournalInput{
		JournalTypeUID: env.deps.JournalType,
		IdempotencyKey: postgrestest.UniqueKey("n1-deferred-honest"),
		Source:         "constraint-timing-control",
		Entries:        entries,
	})
	require.NoError(t, err, "the default mode must be entirely unaffected by 031")
}
