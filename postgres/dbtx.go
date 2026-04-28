package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// DBTX is the minimal database executor accepted by all postgres stores. Both
// *pgxpool.Pool and pgx.Tx satisfy this interface, so callers can bind stores
// to an existing transaction for atomic composition with their own writes.
//
// Use the standard constructors (NewLedgerStore, NewBookingStore, etc.) with a
// *pgxpool.Pool for standalone operation, or call ledger.Service.RunInTx to
// run a closure where every store is automatically rebound to a new
// transaction.
type DBTX interface {
	sqlcgen.DBTX // Exec, Query, QueryRow
	// Begin starts a transaction (on *pgxpool.Pool) or a savepoint (on pgx.Tx).
	Begin(ctx context.Context) (pgx.Tx, error)
}
