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

var _ core.AccountPolicyStore = (*AccountPolicyStore)(nil)

// AccountPolicyStore implements core.AccountPolicyStore using PostgreSQL.
//
// Pool-mode vs tx-mode mirror the other stores in this package: pool mode
// starts its own transaction per write; tx mode (bound via WithDB)
// participates in the caller's transaction.
type AccountPolicyStore struct {
	pool *pgxpool.Pool
	db   DBTX
	q    *sqlcgen.Queries
	dims *dimCache
}

// NewAccountPolicyStore creates a new AccountPolicyStore backed by a
// connection pool.
func NewAccountPolicyStore(pool *pgxpool.Pool) *AccountPolicyStore {
	return &AccountPolicyStore{pool: pool, db: pool, q: sqlcgen.New(pool), dims: dimCacheFor(pool)}
}

// WithDB returns a clone bound to an existing transaction (or any DBTX). The
// caller owns the transaction lifecycle.
func (s *AccountPolicyStore) WithDB(db DBTX) *AccountPolicyStore {
	return &AccountPolicyStore{pool: nil, db: db, q: sqlcgen.New(db), dims: s.dims}
}

// SetPolicy creates or updates the policy at the exact dimension in input,
// appending an audit row (account_policy_changes) in the same transaction.
//
// When input.CurrencyID != 0, SetPolicy acquires the same tx-scoped advisory
// lock that PostJournal/Reserve take for that (holder, currency) pair (see
// acquireBalanceLocks) before reading/writing the policy row. This serializes
// a policy change against any journal/reserve already in flight for that
// exact pair: whichever transaction commits first is guaranteed to be the one
// the other observes when it re-resolves the effective policy under its own
// copy of the lock. A holder-wide policy (CurrencyID == 0, tier 3 in the
// design doc's priority table) cannot be pinned to a single advisory-lock key
// this way -- see docs/INVARIANTS.md I-17 for that caveat.
func (s *AccountPolicyStore) SetPolicy(ctx context.Context, input core.AccountPolicyInput) (*core.AccountPolicy, error) {
	if err := input.Validate(); err != nil {
		return nil, err
	}

	if s.pool == nil {
		// Tx mode: use the caller's transaction directly.
		return s.setPolicyWithQueries(ctx, s.q, input)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: set account policy: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	policy, err := s.setPolicyWithQueries(ctx, s.q.WithTx(tx), input)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: set account policy: commit: %w", err)
	}
	return policy, nil
}

func (s *AccountPolicyStore) setPolicyWithQueries(ctx context.Context, q *sqlcgen.Queries, input core.AccountPolicyInput) (*core.AccountPolicy, error) {
	// Resolve dimension uids to internal ids ("" wildcard -> 0 sentinel).
	currencyID := int64(0)
	if input.CurrencyUID != "" {
		d, err := s.dims.currencyByUIDOrErr(ctx, q, input.CurrencyUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: set account policy: %w", err)
		}
		currencyID = d.ID
	}
	classificationID := int64(0)
	if input.ClassificationUID != "" {
		d, err := s.dims.classByUIDOrErr(ctx, q, input.ClassificationUID)
		if err != nil {
			return nil, fmt.Errorf("postgres: set account policy: %w", err)
		}
		classificationID = d.ID
	}

	if currencyID != 0 {
		if err := acquireBalanceLocks(ctx, q, []balancePair{{holder: input.AccountHolder, currencyID: currencyID}}); err != nil {
			return nil, fmt.Errorf("postgres: set account policy: %w", err)
		}
	}

	before, err := q.GetAccountPolicyForUpdate(ctx, sqlcgen.GetAccountPolicyForUpdateParams{
		AccountHolder:    input.AccountHolder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	hadBefore := true
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: set account policy: load existing: %w", err)
		}
		hadBefore = false
	}

	row, err := q.UpsertAccountPolicy(ctx, sqlcgen.UpsertAccountPolicyParams{
		AccountHolder:     input.AccountHolder,
		CurrencyID:        currencyID,
		ClassificationID:  classificationID,
		Uid:               newUID(),
		Status:            string(input.Status),
		MinBalance:        decimalToNumeric(input.MinBalance),
		EnforceMinBalance: input.EnforceMinBalance,
		Note:              input.Note,
	})
	if err != nil {
		return nil, wrapStoreError("postgres: set account policy: upsert", err)
	}

	oldState, err := s.accountPolicyAuditState(ctx, q, before, hadBefore)
	if err != nil {
		return nil, fmt.Errorf("postgres: set account policy: marshal old state: %w", err)
	}
	newState, err := s.accountPolicyAuditState(ctx, q, row, true)
	if err != nil {
		return nil, fmt.Errorf("postgres: set account policy: marshal new state: %w", err)
	}

	if _, err := q.InsertAccountPolicyChange(ctx, sqlcgen.InsertAccountPolicyChangeParams{
		PolicyID: row.ID,
		OldState: oldState,
		NewState: newState,
		ActorID:  input.ActorID,
	}); err != nil {
		return nil, wrapStoreError("postgres: set account policy: insert audit row", err)
	}

	return accountPolicyFromRow(ctx, s.dims, q, row)
}

// accountPolicyAuditState marshals a policy row for the append-only
// account_policy_changes trail. present=false (no prior row for this
// dimension) marshals to "{}" rather than a zero-valued policy, so the audit
// row honestly reflects "nothing existed yet" instead of a fabricated
// all-zeros state.
func (s *AccountPolicyStore) accountPolicyAuditState(ctx context.Context, q *sqlcgen.Queries, row sqlcgen.AccountPolicy, present bool) ([]byte, error) {
	if !present {
		return []byte("{}"), nil
	}
	policy, err := accountPolicyFromRow(ctx, s.dims, q, row)
	if err != nil {
		return nil, err
	}
	return json.Marshal(policy)
}

// GetPolicy returns the exact-dimension policy row. Returns core.ErrNotFound
// if no row exists at that exact (holder, currency, classification) triple —
// this does NOT do the priority-match resolution the write path uses; use
// GetPolicy for admin/display purposes only.
func (s *AccountPolicyStore) GetPolicy(ctx context.Context, holder int64, currencyUID, classificationUID string) (*core.AccountPolicy, error) {
	currencyID := int64(0)
	if currencyUID != "" {
		d, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
		if err != nil {
			return nil, err
		}
		currencyID = d.ID
	}
	classificationID := int64(0)
	if classificationUID != "" {
		d, err := s.dims.classByUIDOrErr(ctx, s.q, classificationUID)
		if err != nil {
			return nil, err
		}
		classificationID = d.ID
	}
	row, err := s.q.GetAccountPolicy(ctx, sqlcgen.GetAccountPolicyParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: get account policy: %w", core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: get account policy: %w", err)
	}
	return accountPolicyFromRow(ctx, s.dims, s.q, row)
}

// ListPolicies returns every policy row for holder, across all currencies
// and classifications.
func (s *AccountPolicyStore) ListPolicies(ctx context.Context, holder int64) ([]core.AccountPolicy, error) {
	rows, err := s.q.ListAccountPoliciesByHolder(ctx, holder)
	if err != nil {
		return nil, fmt.Errorf("postgres: list account policies: %w", err)
	}
	policies := make([]core.AccountPolicy, len(rows))
	for i, row := range rows {
		policy, err := accountPolicyFromRow(ctx, s.dims, s.q, row)
		if err != nil {
			return nil, err
		}
		policies[i] = *policy
	}
	return policies, nil
}

// getEffectiveAccountPolicy resolves the most specific policy governing
// (holder, currencyID, classificationID) using the priority order documented
// on the GetEffectiveAccountPolicy query: exact > (holder,currency,0) >
// (holder,0,0). Returns (nil, nil) when no policy row matches -- the common
// case, meaning "active, unconstrained". Shared by LedgerStore.PostJournal
// and ReserverStore.Reserve so both write paths resolve policy identically.
func getEffectiveAccountPolicy(ctx context.Context, q *sqlcgen.Queries, holder, currencyID, classificationID int64) (*sqlcgen.AccountPolicy, error) {
	row, err := q.GetEffectiveAccountPolicy(ctx, sqlcgen.GetEffectiveAccountPolicyParams{
		AccountHolder:    holder,
		CurrencyID:       currencyID,
		ClassificationID: classificationID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("postgres: account policy: resolve effective policy: %w", err)
	}
	return &row, nil
}
