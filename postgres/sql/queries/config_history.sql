-- config_history.sql
-- The read side of the forensic trail (docs/INVARIANTS.md I-45, I-58).
--
-- Migration 006 built three tables that answer "who changed the rule that
-- decides where money goes", and nothing ever read them: no query file, no
-- store method, no CLI subcommand, no runbook entry -- only tests. Evidence
-- that is recorded but unreadable leaves "somebody tampered with the config"
-- and "nothing ever happened" indistinguishable on every surface an operator
-- can reach, which is the failure working-agreements §3 is about.
--
-- Ordered newest-first, unlike audit_lists.sql: these are read during an
-- incident, where the question is "what changed recently", not "replay this
-- account's history from the beginning". Keyset cursor is therefore
-- id < cursor, with 0 meaning "first page".

-- name: ListConfigTableChanges :many
-- Change rows for the guarded configuration and lifecycle tables.
-- table_name '' means "every table". since/until zero value (year 0001) is
-- treated as unbounded on that side.
SELECT *
FROM config_table_changes
WHERE (sqlc.arg(table_name)::text = '' OR table_name = sqlc.arg(table_name)::text)
  AND (sqlc.arg(since)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR changed_at >= sqlc.arg(since)::timestamptz)
  AND (sqlc.arg(until)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR changed_at <= sqlc.arg(until)::timestamptz)
  AND (sqlc.arg(cursor_id)::bigint = 0 OR id < sqlc.arg(cursor_id)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListReconcileScanCursorChanges :many
-- Every write to a reconciliation scan cursor. Migration 010's header records
-- what this is for: forging after_holder makes the next scan see zero rows and
-- report Complete=true, and the DB layer's answer to that was detection, not
-- prevention -- which needs a reader to be detection at all.
-- check_name '' means "every check".
SELECT *
FROM reconcile_scan_cursor_changes
WHERE (sqlc.arg(check_name)::text = '' OR check_name = sqlc.arg(check_name)::text)
  AND (sqlc.arg(since)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR changed_at >= sqlc.arg(since)::timestamptz)
  AND (sqlc.arg(until)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR changed_at <= sqlc.arg(until)::timestamptz)
  AND (sqlc.arg(cursor_id)::bigint = 0 OR id < sqlc.arg(cursor_id)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListAccountPolicyChangesByHolder :many
-- (Named ...ByHolder because account_policies.sql already carries a
-- policy_id-scoped ListAccountPolicyChanges. That one has no caller outside
-- the generated code and its own comment calls it a test/audit helper; this is
-- the dimension an incident actually starts from.)
-- The application-written trail for freeze/overdraft policy edits. Unlike the
-- two above it carries a business actor_id, because UpsertAccountPolicy writes
-- it in the caller's transaction -- and unlike them it is therefore only
-- written when the change went through the application. Reading both this and
-- the config_table_changes rows for account_policies is how a change made
-- with raw SQL becomes visible as a change with no operator behind it.
-- account_holder 0 means "every holder".
SELECT apc.*, ap.account_holder, ap.currency_id, ap.classification_id
FROM account_policy_changes apc
JOIN account_policies ap ON ap.id = apc.policy_id
WHERE (sqlc.arg(account_holder)::bigint = 0 OR ap.account_holder = sqlc.arg(account_holder)::bigint)
  AND (sqlc.arg(since)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR apc.created_at >= sqlc.arg(since)::timestamptz)
  AND (sqlc.arg(until)::timestamptz <= '0001-01-02 00:00:00+00'::timestamptz OR apc.created_at <= sqlc.arg(until)::timestamptz)
  AND (sqlc.arg(cursor_id)::bigint = 0 OR apc.id < sqlc.arg(cursor_id)::bigint)
ORDER BY apc.id DESC
LIMIT sqlc.arg(page_limit)::int;
