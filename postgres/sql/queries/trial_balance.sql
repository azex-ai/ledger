-- name: TrialBalanceRows :many
-- Per-classification debit/credit totals for one currency as of a point in
-- time. Filters on (currency_id, effective_at) to use idx_entries_currency_effective
-- (migration 025). Caller computes normal-side net and the global
-- debit=credit check; see docs/plans/2026-07-02-financial-core-hardening-design.md §2.
SELECT
  c.id AS classification_id,
  c.code AS classification_code,
  c.name AS classification_name,
  c.normal_side AS normal_side,
  COALESCE(SUM(CASE WHEN je.entry_type = 'debit' THEN je.amount ELSE 0 END), 0)::numeric AS total_debit,
  COALESCE(SUM(CASE WHEN je.entry_type = 'credit' THEN je.amount ELSE 0 END), 0)::numeric AS total_credit
FROM journal_entries je
JOIN classifications c ON c.id = je.classification_id
WHERE je.currency_id = $1
  AND je.effective_at <= $2
GROUP BY c.id, c.code, c.name, c.normal_side
ORDER BY c.code;
