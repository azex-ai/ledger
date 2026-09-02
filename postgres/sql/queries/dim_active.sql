-- name: InactiveDims :many
-- Reports which of the given dimension ids are soft-deleted (is_active =
-- false). One round trip for all three dimensions; the write path calls this
-- once per journal, after resolving uids to ids.
--
-- Not folded into the dims cache: is_active is the one mutable column on
-- these tables and is deliberately NOT cached (see postgres/dims.go), so
-- this has to be a read. It is also why the check cannot be a foreign key or
-- a CHECK constraint -- both would have to fire on the classifications /
-- currencies / journal_types row, not on the journal referencing it.
--
-- Deactivation is a soft delete: 001_baseline's comment on these tables says
-- so explicitly ("is_active is a soft delete that hides a row from
-- pickers"), and journal_entries keeps its foreign keys forever. What was
-- missing is the other half of "hides a row from pickers" -- nothing
-- actually refused to use one. entry_templates.is_active was enforced
-- (core.EntryTemplate.Render); these three were not, which made
-- DeactivateCurrency / DeactivateClassification / DeactivateJournalType
-- silent no-ops as far as the money path was concerned.
SELECT kind, code FROM (
    SELECT 'currency'::text AS kind, code
    FROM currencies
    WHERE id = ANY(sqlc.arg(currency_ids)::bigint[]) AND is_active = false
    UNION ALL
    SELECT 'classification'::text AS kind, code
    FROM classifications
    WHERE id = ANY(sqlc.arg(classification_ids)::bigint[]) AND is_active = false
    UNION ALL
    SELECT 'journal type'::text AS kind, code
    FROM journal_types
    WHERE id = ANY(sqlc.arg(journal_type_ids)::bigint[]) AND is_active = false
) inactive
ORDER BY kind, code;
