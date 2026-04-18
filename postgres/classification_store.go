package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var (
	_ core.ClassificationStore = (*ClassificationStore)(nil)
	_ core.JournalTypeStore    = (*ClassificationStore)(nil)
)

// ClassificationStore implements ClassificationStore and JournalTypeStore.
type ClassificationStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewClassificationStore creates a new ClassificationStore.
func NewClassificationStore(pool *pgxpool.Pool) *ClassificationStore {
	return &ClassificationStore{
		pool: pool,
		q:    sqlcgen.New(pool),
	}
}

// CreateClassification inserts a new classification.
func (s *ClassificationStore) CreateClassification(ctx context.Context, input core.ClassificationInput) (*core.Classification, error) {
	row, err := s.q.CreateClassification(ctx, sqlcgen.CreateClassificationParams{
		Code:       input.Code,
		Name:       input.Name,
		NormalSide: string(input.NormalSide),
		IsSystem:   input.IsSystem,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: create classification: %w", err)
	}
	return classificationFromRow(row), nil
}

// DeactivateClassification marks a classification as inactive.
func (s *ClassificationStore) DeactivateClassification(ctx context.Context, id int64) error {
	if err := s.q.DeactivateClassification(ctx, id); err != nil {
		return fmt.Errorf("postgres: deactivate classification: %w", err)
	}
	return nil
}

// ListClassifications returns classifications, optionally filtering to active only.
func (s *ClassificationStore) ListClassifications(ctx context.Context, activeOnly bool) ([]core.Classification, error) {
	rows, err := s.q.ListClassifications(ctx, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("postgres: list classifications: %w", err)
	}
	result := make([]core.Classification, len(rows))
	for i, row := range rows {
		result[i] = *classificationFromRow(row)
	}
	return result, nil
}

// CreateJournalType inserts a new journal type.
func (s *ClassificationStore) CreateJournalType(ctx context.Context, input core.JournalTypeInput) (*core.JournalType, error) {
	row, err := s.q.CreateJournalType(ctx, sqlcgen.CreateJournalTypeParams{
		Code: input.Code,
		Name: input.Name,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: create journal type: %w", err)
	}
	return journalTypeFromRow(row), nil
}

// DeactivateJournalType marks a journal type as inactive.
func (s *ClassificationStore) DeactivateJournalType(ctx context.Context, id int64) error {
	if err := s.q.DeactivateJournalType(ctx, id); err != nil {
		return fmt.Errorf("postgres: deactivate journal type: %w", err)
	}
	return nil
}

// ListJournalTypes returns journal types, optionally filtering to active only.
func (s *ClassificationStore) ListJournalTypes(ctx context.Context, activeOnly bool) ([]core.JournalType, error) {
	rows, err := s.q.ListJournalTypes(ctx, activeOnly)
	if err != nil {
		return nil, fmt.Errorf("postgres: list journal types: %w", err)
	}
	result := make([]core.JournalType, len(rows))
	for i, row := range rows {
		result[i] = *journalTypeFromRow(row)
	}
	return result, nil
}
