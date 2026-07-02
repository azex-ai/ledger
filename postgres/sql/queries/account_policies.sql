-- name: UpsertAccountPolicy :one
-- Plain UPSERT keyed on the UNIQUE (account_holder, currency_id,
-- classification_id) dimension. Policy changes are operational config, not
-- funds movement, so this updates in place (the audit trail lives in
-- account_policy_changes, appended by the caller in the same transaction).
INSERT INTO account_policies (
    account_holder, currency_id, classification_id,
    status, min_balance, enforce_min_balance, note
) VALUES (
    $1, $2, $3, $4, $5, $6, $7
)
ON CONFLICT (account_holder, currency_id, classification_id) DO UPDATE SET
    status              = EXCLUDED.status,
    min_balance         = EXCLUDED.min_balance,
    enforce_min_balance = EXCLUDED.enforce_min_balance,
    note                = EXCLUDED.note,
    updated_at          = now()
RETURNING *;

-- name: GetAccountPolicyForUpdate :one
-- Exact-dimension lookup, row-locked, used by SetPolicy to snapshot the
-- pre-change state for the audit row (old_state). No match is a valid case
-- (first time a policy is set for this dimension) — caller treats
-- pgx.ErrNoRows as "no prior state".
SELECT * FROM account_policies
WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3
FOR UPDATE;

-- name: GetAccountPolicy :one
-- Exact-dimension lookup (no priority matching) for admin/read purposes.
SELECT * FROM account_policies
WHERE account_holder = $1 AND currency_id = $2 AND classification_id = $3;

-- name: ListAccountPoliciesByHolder :many
SELECT * FROM account_policies
WHERE account_holder = $1
ORDER BY currency_id, classification_id;

-- name: GetEffectiveAccountPolicy :one
-- Priority match for write-path enforcement (design doc §4): the most
-- specific of (holder,currency,classification) > (holder,currency,0) >
-- (holder,0,0) wins. Specificity score sums 1 per non-wildcard dimension, so
-- a hypothetical (holder,0,classification) row (not a canonical tier, but not
-- forbidden by the schema) ties with (holder,currency,0) — that tie is
-- broken arbitrarily by the second/third ORDER BY key since the design
-- doesn't define an ordering between them.
SELECT * FROM account_policies
WHERE account_holder = $1
  AND currency_id IN ($2, 0)
  AND classification_id IN ($3, 0)
ORDER BY
    (CASE WHEN currency_id <> 0 THEN 1 ELSE 0 END
     + CASE WHEN classification_id <> 0 THEN 1 ELSE 0 END) DESC,
    currency_id DESC,
    classification_id DESC
LIMIT 1;

-- name: InsertAccountPolicyChange :one
INSERT INTO account_policy_changes (policy_id, old_state, new_state, actor_id)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListAccountPolicyChanges :many
-- Test/audit helper — not exposed over HTTP in this phase.
SELECT * FROM account_policy_changes
WHERE policy_id = $1
ORDER BY created_at DESC;
