-- name: CreateTemplate :one
INSERT INTO entry_templates (code, name, journal_type_id)
VALUES ($1, $2, $3)
RETURNING id, code, name, journal_type_id, is_active, created_at;

-- name: CreateTemplateLine :one
INSERT INTO entry_template_lines (template_id, classification_id, entry_type, holder_role, amount_key, sort_order)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, template_id, classification_id, entry_type, holder_role, amount_key, sort_order;

-- name: DeactivateTemplate :exec
UPDATE entry_templates SET is_active = false WHERE id = $1;

-- name: GetTemplateByCode :one
SELECT id, code, name, journal_type_id, is_active, created_at
FROM entry_templates
WHERE code = $1;

-- name: GetTemplateLines :many
SELECT id, template_id, classification_id, entry_type, holder_role, amount_key, sort_order
FROM entry_template_lines
WHERE template_id = $1
ORDER BY sort_order;

-- name: ListTemplates :many
SELECT id, code, name, journal_type_id, is_active, created_at
FROM entry_templates
WHERE (sqlc.arg(active_only)::boolean = false OR is_active = true)
ORDER BY code;
