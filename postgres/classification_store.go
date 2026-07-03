package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var (
	_ core.ClassificationStore = (*ClassificationStore)(nil)
	_ core.JournalTypeStore    = (*ClassificationStore)(nil)
)

// ClassificationStore implements ClassificationStore and JournalTypeStore.
//
// In pool mode (constructed via NewClassificationStore), queries run against
// the pool. In tx mode (bound via withDB), queries participate in the caller's
// transaction.
type ClassificationStore struct {
	// pool is non-nil only in pool mode. Nil signals tx mode.
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

// WithDB returns a clone of the ClassificationStore bound to an existing
// transaction.
func (s *ClassificationStore) WithDB(db DBTX) *ClassificationStore {
	return &ClassificationStore{
		pool: nil, // tx mode
		q:    sqlcgen.New(db),
	}
}

// CreateClassification inserts a new classification.
func (s *ClassificationStore) CreateClassification(ctx context.Context, input core.ClassificationInput) (*core.Classification, error) {
	if input.Code == "" || input.Name == "" {
		return nil, fmt.Errorf("postgres: create classification: code and name required: %w", core.ErrInvalidInput)
	}
	if !input.NormalSide.IsValid() {
		return nil, fmt.Errorf("postgres: create classification: invalid normal side %q: %w", input.NormalSide, core.ErrInvalidInput)
	}
	if !input.BalanceRole.IsValid() {
		return nil, fmt.Errorf("postgres: create classification: invalid balance role %q: %w", input.BalanceRole, core.ErrInvalidInput)
	}

	var lifecycle []byte
	if input.Lifecycle != nil {
		if err := input.Lifecycle.Validate(); err != nil {
			return nil, fmt.Errorf("postgres: create classification: invalid lifecycle: %w", err)
		}
		var err error
		lifecycle, err = json.Marshal(input.Lifecycle)
		if err != nil {
			return nil, fmt.Errorf("postgres: create classification: marshal lifecycle: %w", err)
		}
	} else {
		lifecycle = []byte("{}")
	}
	row, err := s.q.CreateClassification(ctx, sqlcgen.CreateClassificationParams{
		Code:        input.Code,
		Name:        input.Name,
		NormalSide:  string(input.NormalSide),
		IsSystem:    input.IsSystem,
		Lifecycle:   lifecycle,
		Uid:         newUID(),
		BalanceRole: string(input.BalanceRole),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: create classification", err)
	}
	return classificationFromRow(row), nil
}

// SetBalanceRole retags a classification's balance role. Intended for
// expand-style upgrades ('' -> role); switching between two non-empty roles
// re-buckets historical balances in the breakdown view — the caller owns that
// decision (presets only ever upgrade from '').
func (s *ClassificationStore) SetBalanceRole(ctx context.Context, uid string, role core.BalanceRole) error {
	if !role.IsValid() {
		return fmt.Errorf("postgres: set balance role: invalid balance role %q: %w", role, core.ErrInvalidInput)
	}
	pgUID, err := uidToPG(uid)
	if err != nil {
		return err
	}
	if err := s.q.SetClassificationBalanceRole(ctx, sqlcgen.SetClassificationBalanceRoleParams{
		Uid:         pgUID,
		BalanceRole: string(role),
	}); err != nil {
		return wrapStoreError("postgres: set balance role", err)
	}
	return nil
}

// GetByCode returns a classification by its unique code.
func (s *ClassificationStore) GetByCode(ctx context.Context, code string) (*core.Classification, error) {
	row, err := s.q.GetClassificationByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get classification by code %q: %w", code, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get classification by code %q: %w", code, err)
	}
	return classificationFromRow(row), nil
}

// DeactivateClassification marks a classification as inactive.
func (s *ClassificationStore) DeactivateClassification(ctx context.Context, uid string) error {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return err
	}
	if err := s.q.DeactivateClassification(ctx, pgUID); err != nil {
		return wrapStoreError("postgres: deactivate classification", err)
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
		Uid:  newUID(),
	})
	if err != nil {
		return nil, wrapStoreError("postgres: create journal type", err)
	}
	return journalTypeFromRow(row), nil
}

// GetJournalTypeByCode returns a journal type by its unique code.
func (s *ClassificationStore) GetJournalTypeByCode(ctx context.Context, code string) (*core.JournalType, error) {
	row, err := s.q.GetJournalTypeByCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get journal type by code %q: %w", code, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get journal type by code %q: %w", code, err)
	}
	return journalTypeFromRow(row), nil
}

// DeactivateJournalType marks a journal type as inactive.
func (s *ClassificationStore) DeactivateJournalType(ctx context.Context, uid string) error {
	pgUID, err := uidToPG(uid)
	if err != nil {
		return err
	}
	if err := s.q.DeactivateJournalType(ctx, pgUID); err != nil {
		return wrapStoreError("postgres: deactivate journal type", err)
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
