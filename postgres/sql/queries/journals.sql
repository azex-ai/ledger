-- name: InsertJournal :one
-- auth_digest/auth_signature/auth_key_id (migration 046, P5) are empty when
-- the caller did not sign this posting. auth_status (migration 051, design
-- doc §7.5) always records WHY: 'signed' / 'unsigned_no_attestor' /
-- 'unsigned_tx_mode' -- see core.AuthStatus.
INSERT INTO journals (journal_type_id, idempotency_key, total_debit, total_credit, metadata, actor_id, source, reversal_of, event_id, effective_at, uid, auth_digest, auth_signature, auth_key_id, auth_status)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING *;

-- name: InsertJournalEntry :one
-- Column order in RETURNING matches the table's physical column order
-- (effective_at was appended after created_at in migration 025) so sqlc
-- matches the generated row to the JournalEntry model instead of minting a
-- distinct one-off row type.
INSERT INTO journal_entries (journal_id, account_holder, currency_id, classification_id, entry_type, amount, effective_at, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, now())
RETURNING id, journal_id, account_holder, currency_id, classification_id, entry_type, amount, created_at, effective_at;

-- name: GetJournal :one
SELECT * FROM journals WHERE id = $1;

-- name: GetJournalForUpdate :one
-- Row-locks the original journal for the duration of the caller's
-- transaction, serializing concurrent ReverseJournalFraction calls against
-- the same journal so their cumulative-reversed-amount checks cannot race.
SELECT * FROM journals WHERE id = $1 FOR UPDATE;

-- name: GetJournalByIdempotencyKey :one
SELECT * FROM journals WHERE idempotency_key = $1;

-- name: ListReversalsByOriginalJournalID :many
-- A journal may now have more than one reversal (partial reversals), so this
-- returns all of them, oldest first. Replaces the old GetReversalByOriginalJournalID
-- :one query, which silently returned an arbitrary single row once multiple
-- reversals became possible.
SELECT * FROM journals WHERE reversal_of = $1 ORDER BY id;

-- name: ListReversalEntriesByOriginal :many
-- All entries across every reversal journal (full or partial) of the given
-- original journal. Used to compute the cumulative amount already reversed
-- per (holder, currency, classification, original entry_type) dimension
-- before allowing another partial reversal.
SELECT je.id, je.journal_id, je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount, je.created_at
FROM journal_entries je
JOIN journals j ON j.id = je.journal_id
WHERE j.reversal_of = $1
ORDER BY je.id;

-- name: ListJournalEntries :many
-- Column order matches the table's physical order (see InsertJournalEntry).
-- j.uid rides along so the mapper can emit journal_uid without a per-row lookup.
SELECT je.id, je.journal_id, je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount, je.created_at, je.effective_at, j.uid AS journal_uid
FROM journal_entries je
JOIN journals j ON j.id = je.journal_id
WHERE je.journal_id = $1
ORDER BY je.id;

-- name: ListEntriesByAccount :many
-- Column order matches the table's physical order (see InsertJournalEntry).
SELECT je.id, je.journal_id, je.account_holder, je.currency_id, je.classification_id, je.entry_type, je.amount, je.created_at, je.effective_at, j.uid AS journal_uid
FROM journal_entries je
JOIN journals j ON j.id = je.journal_id
WHERE je.account_holder = $1 AND je.currency_id = $2
  AND je.id > sqlc.arg(cursor_id)::bigint
ORDER BY je.id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: SumEntriesSinceCheckpoint :many
SELECT
  classification_id,
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND id > sqlc.arg(since_entry_id)::bigint
GROUP BY classification_id, entry_type;

-- name: DistinctClassificationsForAccount :many
SELECT DISTINCT classification_id
FROM journal_entries
WHERE account_holder = $1 AND currency_id = $2
ORDER BY classification_id;

-- name: SumEntriesSinceForClassification :many
SELECT
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
WHERE account_holder = $1
  AND currency_id = $2
  AND classification_id = $3
  AND id > sqlc.arg(since_entry_id)::bigint
GROUP BY entry_type;

-- name: ListJournalsCursor :many
SELECT * FROM journals
WHERE id > sqlc.arg(cursor_id)::bigint
ORDER BY id ASC
LIMIT sqlc.arg(page_limit)::int;

-- name: SumGlobalDebitCredit :many
SELECT
  entry_type,
  COALESCE(SUM(amount), 0) as total
FROM journal_entries
GROUP BY entry_type;

-- name: SumEntriesByAccountClassification :many
-- Reconcile Check#2 comparison basis: each classification's entries are
-- bounded by that classification's OWN checkpoint watermark
-- (je.id <= cp.last_entry_id), so checkpoint.balance vs this sum is an exact
-- invariant regardless of in-flight rollups. An unbounded sum would flag
-- every account with unmaterialized entries as "drift" — permanent false
-- positives on high-frequency system accounts. Classifications with no
-- checkpoint yet are excluded (their checkpoint balance is implicitly zero
-- over an empty prefix — nothing to compare).
SELECT
  je.classification_id,
  je.entry_type,
  COALESCE(SUM(je.amount), 0) as total
FROM journal_entries je
JOIN balance_checkpoints cp
  ON cp.account_holder = je.account_holder
 AND cp.currency_id = je.currency_id
 AND cp.classification_id = je.classification_id
WHERE je.account_holder = $1
  AND je.currency_id = $2
  AND je.id <= cp.last_entry_id
GROUP BY je.classification_id, je.entry_type;

-- name: VerifyJournalBalanced :one
-- Returns the first currency_id that does not net to zero across the journal's
-- entries, or NULL if the journal is balanced. Run inside the same transaction
-- as the entry inserts, before commit, so a failure rolls back cleanly with a
-- precise "which currency" error. This is the application-layer half of the
-- check; migration 044 restores a DB-layer deferred constraint trigger as the
-- backstop for callers that bypass this query (e.g. direct SQL).
SELECT currency_id
FROM journal_entries
WHERE journal_id = $1
GROUP BY currency_id
HAVING SUM(CASE WHEN entry_type = 'debit' THEN amount ELSE -amount END) <> 0
LIMIT 1;

-- name: AcquireBalanceLock :exec
-- Take a transaction-scoped advisory lock keyed on (holder, currency_id) so
-- concurrent reserves and journal posts that touch the same pair serialize.
-- The caller passes a stable composite text key (e.g. "balance:<holder>:<currency_id>").
--
-- Two-key form pg_advisory_xact_lock(int4, int4) with a fixed namespace (1)
-- in the first slot -- NOT the single-arg bigint form. PostgreSQL's advisory
-- lock tag carries a 4th field that distinguishes the two-key API from the
-- single-key API, so pg_advisory_xact_lock(bigint) and
-- pg_advisory_xact_lock(int4, int4) are genuinely disjoint lock spaces no
-- matter what values are passed; the two-key space is additionally
-- partitioned by its first argument. This namespace (1) can therefore never
-- collide with AcquireIdempotencyLock's namespace (2) below, even though a
-- caller fully controls the idempotency_key string end to end
-- (server/handler_journals.go accepts it with no format restriction) and
-- could otherwise pick a string like "balance:1:1" to alias a real balance
-- pair -- see concurrency.md's ABBA-deadlock finding this closes. Hash
-- collisions *within* this namespace (two different (holder,currency_id)
-- pairs landing on the same hashtext() value) only reduce concurrency, they
-- do not affect correctness.
SELECT pg_advisory_xact_lock(1::int4, hashtext(sqlc.arg(key)::text));

-- name: AcquireIdempotencyLock :exec
-- Serialize concurrent requests that present the same idempotency key, even if
-- they touch different account dimensions. Namespace 2 in the two-key
-- advisory lock form -- see AcquireBalanceLock's comment for why this cannot
-- collide with the balance-lock namespace regardless of the key string a
-- caller supplies. Collisions within this namespace only reduce concurrency;
-- they do not affect correctness.
SELECT pg_advisory_xact_lock(2::int4, hashtext(sqlc.arg(key)::text));

-- name: GetJournalByUID :one
SELECT * FROM journals WHERE uid = $1;

-- name: GetJournalForUpdateByUID :one
SELECT * FROM journals WHERE uid = $1 FOR UPDATE;

-- name: GetJournalUIDByID :one
SELECT uid FROM journals WHERE id = $1;
