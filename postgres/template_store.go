package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var _ core.TemplateStore = (*TemplateStore)(nil)

// TemplateStore implements core.TemplateStore using PostgreSQL.
type TemplateStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewTemplateStore creates a new TemplateStore.
func NewTemplateStore(pool *pgxpool.Pool) *TemplateStore {
	return &TemplateStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// CreateTemplate inserts a template with its lines in a transaction.
func (s *TemplateStore) CreateTemplate(ctx context.Context, input core.TemplateInput) (*core.EntryTemplate, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: create template: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	tmpl, err := qtx.CreateTemplate(ctx, sqlcgen.CreateTemplateParams{
		Code:          input.Code,
		Name:          input.Name,
		JournalTypeID: input.JournalTypeID,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: create template: insert: %w", err)
	}

	sqlcLines := make([]sqlcgen.EntryTemplateLine, len(input.Lines))
	for i, l := range input.Lines {
		line, err := qtx.CreateTemplateLine(ctx, sqlcgen.CreateTemplateLineParams{
			TemplateID:       tmpl.ID,
			ClassificationID: l.ClassificationID,
			EntryType:        string(l.EntryType),
			HolderRole:       string(l.HolderRole),
			AmountKey:        l.AmountKey,
			SortOrder:        int32(l.SortOrder),
		})
		if err != nil {
			return nil, fmt.Errorf("postgres: create template: insert line[%d]: %w", i, err)
		}
		sqlcLines[i] = line
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: create template: commit: %w", err)
	}

	return templateFromRow(tmpl, sqlcLines), nil
}

// DeactivateTemplate marks a template as inactive.
func (s *TemplateStore) DeactivateTemplate(ctx context.Context, id int64) error {
	if err := s.q.DeactivateTemplate(ctx, id); err != nil {
		return fmt.Errorf("postgres: deactivate template: %w", err)
	}
	return nil
}

// GetTemplate retrieves a template by code with its lines.
func (s *TemplateStore) GetTemplate(ctx context.Context, code string) (*core.EntryTemplate, error) {
	tmpl, err := s.q.GetTemplateByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get template: %q not found", code)
		}
		return nil, fmt.Errorf("postgres: get template: %w", err)
	}

	lines, err := s.q.GetTemplateLines(ctx, tmpl.ID)
	if err != nil {
		return nil, fmt.Errorf("postgres: get template: lines: %w", err)
	}

	return templateFromRow(tmpl, lines), nil
}

// ListTemplates returns templates, optionally filtering to active only.
func (s *TemplateStore) ListTemplates(ctx context.Context, activeOnly bool) ([]core.EntryTemplate, error) {
	rows, err := s.q.ListTemplates(ctx, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("postgres: list templates: %w", err)
	}

	result := make([]core.EntryTemplate, len(rows))
	for i, row := range rows {
		lines, err := s.q.GetTemplateLines(ctx, row.ID)
		if err != nil {
			return nil, fmt.Errorf("postgres: list templates: lines for %d: %w", row.ID, err)
		}
		result[i] = *templateFromRow(row, lines)
	}
	return result, nil
}
