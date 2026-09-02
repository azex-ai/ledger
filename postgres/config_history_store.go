package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// ConfigHistoryStore implements core.ConfigChangeReader: the read side of the
// three forensic tables migration 006 and 010 introduced.
//
// Separate from AuditStore deliberately. AuditStore answers questions about
// the ledger's contents -- journals, entries, bookings, reversals. This
// answers a question about the ledger's *rules*, and during an incident the
// two are asked by different people for different reasons. Keeping them apart
// also keeps the port small enough that a consumer can grant it to a read-only
// investigation tool without handing over the balance surface as well.
type ConfigHistoryStore struct {
	q *sqlcgen.Queries
}

// Compile-time check.
var _ core.ConfigChangeReader = (*ConfigHistoryStore)(nil)

// NewConfigHistoryStore creates a ConfigHistoryStore backed by a pool.
func NewConfigHistoryStore(pool *pgxpool.Pool) *ConfigHistoryStore {
	return &ConfigHistoryStore{q: sqlcgen.New(pool)}
}

// WithDB returns a clone bound to an existing transaction.
func (s *ConfigHistoryStore) WithDB(db DBTX) *ConfigHistoryStore {
	return &ConfigHistoryStore{q: sqlcgen.New(db)}
}

// defaultConfigHistoryLimit matches AuditStore's: enough rows to see a pattern
// in one page, small enough that a query against a flooded audit table returns.
const defaultConfigHistoryLimit = 50

func configHistoryLimit(l int32) int32 {
	if l <= 0 || l > 500 {
		return defaultConfigHistoryLimit
	}
	return l
}

// ListConfigChanges returns changes to the guarded configuration and
// lifecycle tables, newest first. TableName "" means every table.
func (s *ConfigHistoryStore) ListConfigChanges(ctx context.Context, filter core.ConfigChangeFilter) ([]core.ConfigChange, string, error) {
	cursorID, err := decodeAuditCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := configHistoryLimit(filter.Limit)

	rows, err := s.q.ListConfigTableChanges(ctx, sqlcgen.ListConfigTableChangesParams{
		TableName: filter.TableName,
		Since:     sinceOrEpoch(filter.Since),
		Until:     sinceOrEpoch(filter.Until),
		CursorID:  cursorID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: config history: list config changes: %w", err)
	}

	out := make([]core.ConfigChange, len(rows))
	for i, r := range rows {
		out[i] = core.ConfigChange{
			TableName: r.TableName,
			OldRow:    r.OldRow,
			NewRow:    r.NewRow,
			ChangedBy: r.ChangedBy,
			ChangedAt: r.ChangedAt,
		}
	}

	next := ""
	if len(rows) == int(limit) {
		next = encodeCursorString(rows[len(rows)-1].ID)
	}
	return out, next, nil
}

// ListScanCursorChanges returns writes to reconciliation scan cursors, newest
// first. CheckName "" means every check.
func (s *ConfigHistoryStore) ListScanCursorChanges(ctx context.Context, filter core.ConfigChangeFilter) ([]core.ScanCursorChange, string, error) {
	cursorID, err := decodeAuditCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := configHistoryLimit(filter.Limit)

	rows, err := s.q.ListReconcileScanCursorChanges(ctx, sqlcgen.ListReconcileScanCursorChangesParams{
		CheckName: filter.CheckName,
		Since:     sinceOrEpoch(filter.Since),
		Until:     sinceOrEpoch(filter.Until),
		CursorID:  cursorID,
		PageLimit: limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: config history: list scan cursor changes: %w", err)
	}

	out := make([]core.ScanCursorChange, len(rows))
	for i, r := range rows {
		out[i] = core.ScanCursorChange{
			CheckName:        r.CheckName,
			OldAfterHolder:   r.OldAfterHolder,
			OldAfterCurrency: r.OldAfterCurrency,
			OldLapDirty:      r.OldLapDirty,
			NewAfterHolder:   r.NewAfterHolder,
			NewAfterCurrency: r.NewAfterCurrency,
			NewLapDirty:      r.NewLapDirty,
			ChangedBy:        r.ChangedBy,
			ChangedAt:        r.ChangedAt,
		}
	}

	next := ""
	if len(rows) == int(limit) {
		next = encodeCursorString(rows[len(rows)-1].ID)
	}
	return out, next, nil
}

// ListAccountPolicyChanges returns application-recorded freeze/overdraft
// policy edits, newest first. AccountHolder 0 means every holder.
func (s *ConfigHistoryStore) ListAccountPolicyChanges(ctx context.Context, filter core.ConfigChangeFilter) ([]core.AccountPolicyChange, string, error) {
	cursorID, err := decodeAuditCursor(filter.Cursor)
	if err != nil {
		return nil, "", err
	}
	limit := configHistoryLimit(filter.Limit)

	rows, err := s.q.ListAccountPolicyChangesByHolder(ctx, sqlcgen.ListAccountPolicyChangesByHolderParams{
		AccountHolder: filter.AccountHolder,
		Since:         sinceOrEpoch(filter.Since),
		Until:         sinceOrEpoch(filter.Until),
		CursorID:      cursorID,
		PageLimit:     limit,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: config history: list account policy changes: %w", err)
	}

	out := make([]core.AccountPolicyChange, len(rows))
	for i, r := range rows {
		out[i] = core.AccountPolicyChange{
			AccountHolder:     r.AccountHolder,
			CurrencyUID:       r.CurrencyUid,
			ClassificationUID: r.ClassificationUid,
			OldState:          r.OldState,
			NewState:          r.NewState,
			ActorID:           r.ActorID,
			CreatedAt:         r.CreatedAt,
		}
	}

	next := ""
	if len(rows) == int(limit) {
		next = encodeCursorString(rows[len(rows)-1].ID)
	}
	return out, next, nil
}
