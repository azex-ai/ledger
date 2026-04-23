package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

var (
	_ core.Operator        = (*OperationStore)(nil)
	_ core.OperationReader = (*OperationStore)(nil)
)

// OperationStore implements core.Operator and core.OperationReader using PostgreSQL.
type OperationStore struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

// NewOperationStore creates a new OperationStore.
func NewOperationStore(pool *pgxpool.Pool, q *sqlcgen.Queries) *OperationStore {
	return &OperationStore{pool: pool, q: q}
}

// CreateOperation creates a new operation with initial status from the classification lifecycle.
// Idempotent: returns existing operation if idempotency_key already exists.
func (s *OperationStore) CreateOperation(ctx context.Context, input core.CreateOperationInput) (*core.Operation, error) {
	// Check idempotency
	existing, err := s.q.GetOperationByIdempotencyKey(ctx, input.IdempotencyKey)
	if err == nil {
		return operationFromRow(existing), nil
	}

	// Load classification to get lifecycle
	class, err := s.q.GetClassificationByCode(ctx, input.ClassificationCode)
	if err != nil {
		return nil, fmt.Errorf("postgres: create operation: classification %q: %w", input.ClassificationCode, err)
	}

	var lifecycle core.Lifecycle
	if len(class.Lifecycle) <= 2 {
		return nil, fmt.Errorf("postgres: create operation: classification %q has no lifecycle", input.ClassificationCode)
	}
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: create operation: unmarshal lifecycle: %w", err)
	}

	row, err := s.q.InsertOperation(ctx, sqlcgen.InsertOperationParams{
		ClassificationID: class.ID,
		AccountHolder:    input.AccountHolder,
		CurrencyID:       input.CurrencyID,
		Amount:           decimalToNumeric(input.Amount),
		Status:           string(lifecycle.Initial),
		ChannelName:      input.ChannelName,
		IdempotencyKey:   input.IdempotencyKey,
		Metadata:         anyMetadataToJSON(input.Metadata),
		ExpiresAt:        input.ExpiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: create operation: %w", err)
	}
	return operationFromRow(row), nil
}

// Transition advances an operation's status and records an event atomically.
func (s *OperationStore) Transition(ctx context.Context, input core.TransitionInput) (*core.Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: begin: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)

	// Lock operation
	op, err := qtx.GetOperationForUpdate(ctx, input.OperationID)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: get operation %d: %w", input.OperationID, err)
	}

	// Load classification for lifecycle validation
	class, err := qtx.GetClassification(ctx, op.ClassificationID)
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: get classification: %w", err)
	}

	var lifecycle core.Lifecycle
	if err := json.Unmarshal(class.Lifecycle, &lifecycle); err != nil {
		return nil, fmt.Errorf("postgres: transition: unmarshal lifecycle: %w", err)
	}

	fromStatus := core.Status(op.Status)
	if !lifecycle.CanTransition(fromStatus, input.ToStatus) {
		return nil, fmt.Errorf("postgres: transition: %w: %s -> %s", core.ErrInvalidTransition, op.Status, input.ToStatus)
	}

	// Merge metadata
	metadata := jsonToAnyMetadata(op.Metadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for k, v := range input.Metadata {
		metadata[k] = v
	}

	// Determine settled_amount: use input if non-zero, else keep existing
	settledAmount := mustNumericToDecimal(op.SettledAmount)
	if !input.Amount.IsZero() {
		settledAmount = input.Amount
	}

	// Determine channel_ref
	channelRef := op.ChannelRef
	if input.ChannelRef != "" {
		channelRef = input.ChannelRef
	}

	// Update operation
	err = qtx.UpdateOperationTransition(ctx, sqlcgen.UpdateOperationTransitionParams{
		ID:            op.ID,
		Status:        string(input.ToStatus),
		ChannelRef:    channelRef,
		SettledAmount: decimalToNumeric(settledAmount),
		JournalID:     op.JournalID,
		Metadata:      anyMetadataToJSON(metadata),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: update: %w", err)
	}

	// Insert event (atomic with transition)
	eventRow, err := qtx.InsertEvent(ctx, sqlcgen.InsertEventParams{
		ClassificationCode: class.Code,
		OperationID:        op.ID,
		AccountHolder:      op.AccountHolder,
		CurrencyID:         op.CurrencyID,
		FromStatus:         op.Status,
		ToStatus:           string(input.ToStatus),
		Amount:             op.Amount,
		SettledAmount:      decimalToNumeric(settledAmount),
		JournalID:          0,
		Metadata:           anyMetadataToJSON(metadata),
		OccurredAt:         time.Now(),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: transition: insert event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: transition: commit: %w", err)
	}

	return eventFromRow(eventRow), nil
}

// GetOperation returns an operation by ID.
func (s *OperationStore) GetOperation(ctx context.Context, id int64) (*core.Operation, error) {
	row, err := s.q.GetOperation(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("postgres: get operation %d: %w", id, err)
	}
	return operationFromRow(row), nil
}

// ListExpiredOperations returns non-terminal operations past their expiration time.
func (s *OperationStore) ListExpiredOperations(ctx context.Context, limit int) ([]core.Operation, error) {
	rows, err := s.q.ListExpiredOperations(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: list expired operations: %w", err)
	}
	ops := make([]core.Operation, len(rows))
	for i, row := range rows {
		ops[i] = *operationFromRow(row)
	}
	return ops, nil
}

// ListOperations returns operations matching the filter.
func (s *OperationStore) ListOperations(ctx context.Context, filter core.OperationFilter) ([]core.Operation, error) {
	rows, err := s.q.ListOperationsByFilter(ctx, sqlcgen.ListOperationsByFilterParams{
		AccountHolder:    filter.AccountHolder,
		ClassificationID: filter.ClassificationID,
		Status:           filter.Status,
		ID:               filter.Cursor,
		Limit:            int32(filter.Limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list operations: %w", err)
	}
	ops := make([]core.Operation, len(rows))
	for i, row := range rows {
		ops[i] = *operationFromRow(row)
	}
	return ops, nil
}
