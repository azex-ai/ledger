// Package postgres — dims.go
// In-process id<->uid resolution for the three config dimensions (currencies,
// classifications, journal types). uid is the only identifier in any public
// contract (api-contract §3); internal BIGSERIAL ids are storage details, so
// every store boundary crossing resolves through here.
//
// Cache safety has two independent dimensions, and only the first used to be
// argued here:
//
//  1. Field immutability. Config rows are insert-only for the cached fields
//     (id, uid, code, normal_side, exponent are all immutable after
//     creation; only is_active mutates, and it is deliberately NOT cached,
//     so a stale entry can never report the wrong id, code or precision).
//
//  2. Entry EXISTENCE. A cached entry asserts "this row exists", and that is
//     a claim about committed state. The shared cache is keyed by pool and
//     inherited by every tx-bound clone, so a refresh driven from inside a
//     caller's open transaction used to publish that transaction's
//     uncommitted config rows to the whole process. If the transaction then
//     rolled back, the row was gone but the cache entry stayed — and because
//     a cache HIT never re-validates, nothing ever healed it: every later
//     request on any connection resolved that uid to a burnt BIGSERIAL id
//     and failed on a foreign key, until the process restarted
//     (concurrency.md 2026-09-02 B-m5).
//
// A tx-bound store clone therefore gets its own tx-scoped VIEW of the cache
// (dimCacheForTx) instead of the shared pointer: it reads through to the
// pool-wide cache, so a warm parent costs it nothing, but anything it has to
// go to the database for lands in its own maps and dies with the clone.
// Nothing a transaction can see before it commits ever becomes visible to
// the rest of the process. The pool-wide cache is only ever refreshed from
// outside a caller's transaction, so it holds committed rows by
// construction.
//
// Note what this deliberately does NOT do: refresh the shared cache through
// a second pool connection. That was the first shape of this fix, and it
// deadlocks — the refresh runs while the caller already holds a pool
// connection for its open transaction, so N concurrent cold-cache writers
// on an N-connection pool all wait for a connection none of them will
// release (reproduced by TestIdempotency_ConcurrentSameKey with 100
// goroutines). A cache refresh must never need a connection the caller does
// not already have.
//
// A miss triggers one full-table refresh — these tables are small by design.
package postgres

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

type dimCurrency struct {
	ID       int64
	UID      string
	Code     string
	Exponent int32
}

type dimClassification struct {
	ID         int64
	UID        string
	Code       string
	NormalSide core.NormalSide
}

type dimJournalType struct {
	ID   int64
	UID  string
	Code string
}

type dimCache struct {
	mu sync.RWMutex

	// parent is the pool-wide cache a tx-scoped view reads through, or nil
	// for the pool-wide cache itself. A view never writes to its parent —
	// that is what keeps uncommitted rows out of process-wide state.
	parent *dimCache

	currencyByUID map[string]dimCurrency
	currencyByID  map[int64]dimCurrency
	classByUID    map[string]dimClassification
	classByID     map[int64]dimClassification
	jtByUID       map[string]dimJournalType
	jtByID        map[int64]dimJournalType
}

// dimCaches is keyed by pool so parallel tests against different databases
// never see each other's ids. Tx-bound store clones carry the pointer they
// inherited from their pool-backed parent.
var dimCaches sync.Map // *pgxpool.Pool -> *dimCache

func dimCacheFor(pool *pgxpool.Pool) *dimCache {
	if pool == nil {
		return &dimCache{}
	}
	if c, ok := dimCaches.Load(pool); ok {
		return c.(*dimCache)
	}
	c, _ := dimCaches.LoadOrStore(pool, &dimCache{})
	return c.(*dimCache)
}

// dimCacheForTx returns a tx-scoped view over parent, for a store clone bound
// to a caller's transaction (WithDB). Reads fall through to parent, so a warm
// pool-wide cache serves them with no extra query; anything this view has to
// resolve itself — notably a config row the caller created in this very
// transaction — stays in the view and is discarded with it.
//
// Every store that carries a *dimCache MUST use this in WithDB rather than
// passing s.dims through. Sharing the pointer is what let an uncommitted row
// become process-wide state (concurrency.md 2026-09-02 B-m5).
func dimCacheForTx(parent *dimCache) *dimCache {
	return &dimCache{parent: parent}
}

// refresh reloads all three dimension tables into c from q.
//
// c is either a tx-scoped view (q is the caller's transaction; the rows land
// in the view and die with it) or the pool-wide cache (q never belongs to a
// caller's transaction, because tx-bound clones hold views — see
// dimCacheForTx). Either way this uses the connection the caller already
// has: see the package doc for why acquiring a second one deadlocks.
func (c *dimCache) refresh(ctx context.Context, q *sqlcgen.Queries) error {
	curs, err := q.ListCurrencyDims(ctx)
	if err != nil {
		return fmt.Errorf("postgres: dims: list currencies: %w", err)
	}
	classes, err := q.ListClassificationDims(ctx)
	if err != nil {
		return fmt.Errorf("postgres: dims: list classifications: %w", err)
	}
	jts, err := q.ListJournalTypeDims(ctx)
	if err != nil {
		return fmt.Errorf("postgres: dims: list journal types: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.currencyByUID = make(map[string]dimCurrency, len(curs))
	c.currencyByID = make(map[int64]dimCurrency, len(curs))
	for _, r := range curs {
		d := dimCurrency{ID: r.ID, UID: pgToUID(r.Uid), Code: r.Code, Exponent: int32(r.Exponent)}
		c.currencyByUID[d.UID] = d
		c.currencyByID[d.ID] = d
	}
	c.classByUID = make(map[string]dimClassification, len(classes))
	c.classByID = make(map[int64]dimClassification, len(classes))
	for _, r := range classes {
		d := dimClassification{ID: r.ID, UID: pgToUID(r.Uid), Code: r.Code, NormalSide: core.NormalSide(r.NormalSide)}
		c.classByUID[d.UID] = d
		c.classByID[d.ID] = d
	}
	c.jtByUID = make(map[string]dimJournalType, len(jts))
	c.jtByID = make(map[int64]dimJournalType, len(jts))
	for _, r := range jts {
		d := dimJournalType{ID: r.ID, UID: pgToUID(r.Uid), Code: r.Code}
		c.jtByUID[d.UID] = d
		c.jtByID[d.ID] = d
	}
	return nil
}

// dimLookup reads c (and, for a tx-scoped view, its parent) under the read
// lock; on a miss it refreshes c once from q and retries. The second miss is
// the caller's ErrNotFound.
//
// pick takes the cache to read from so the same accessor can be applied to
// the view and to its parent.
func dimLookup[K comparable, V any](ctx context.Context, c *dimCache, q *sqlcgen.Queries, pick func(*dimCache) map[K]V, key K) (V, bool, error) {
	if v, ok := dimRead(c, pick, key); ok {
		return v, true, nil
	}
	if err := c.refresh(ctx, q); err != nil {
		var zero V
		return zero, false, err
	}
	v, ok := dimRead(c, pick, key)
	return v, ok, nil
}

// dimRead looks key up in c, falling through to c.parent when c is a
// tx-scoped view. Only a view's own maps are ever written, so falling
// through is a pure read of the pool-wide cache.
func dimRead[K comparable, V any](c *dimCache, pick func(*dimCache) map[K]V, key K) (V, bool) {
	c.mu.RLock()
	v, ok := pick(c)[key]
	c.mu.RUnlock()
	if ok || c.parent == nil {
		return v, ok
	}
	return dimRead(c.parent, pick, key)
}

func (c *dimCache) currencyByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimCurrency, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[string]dimCurrency { return dc.currencyByUID }, uid)
	if err != nil {
		return dimCurrency{}, err
	}
	if !ok {
		return dimCurrency{}, fmt.Errorf("postgres: currency %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) currencyByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimCurrency, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[int64]dimCurrency { return dc.currencyByID }, id)
	if err != nil {
		return dimCurrency{}, err
	}
	if !ok {
		return dimCurrency{}, fmt.Errorf("postgres: currency id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) classByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimClassification, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[string]dimClassification { return dc.classByUID }, uid)
	if err != nil {
		return dimClassification{}, err
	}
	if !ok {
		return dimClassification{}, fmt.Errorf("postgres: classification %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) classByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimClassification, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[int64]dimClassification { return dc.classByID }, id)
	if err != nil {
		return dimClassification{}, err
	}
	if !ok {
		return dimClassification{}, fmt.Errorf("postgres: classification id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) jtByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimJournalType, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[string]dimJournalType { return dc.jtByUID }, uid)
	if err != nil {
		return dimJournalType{}, err
	}
	if !ok {
		return dimJournalType{}, fmt.Errorf("postgres: journal type %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) jtByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimJournalType, error) {
	v, ok, err := dimLookup(ctx, c, q, func(dc *dimCache) map[int64]dimJournalType { return dc.jtByID }, id)
	if err != nil {
		return dimJournalType{}, err
	}
	if !ok {
		return dimJournalType{}, fmt.Errorf("postgres: journal type id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}

// assertDimsActive refuses a write whose dimensions include a soft-deleted
// currency, classification or journal type (B-X1).
//
// DeactivateCurrency / DeactivateClassification / DeactivateJournalType
// existed, were documented as soft deletes that "hide a row from pickers",
// and were checked by nothing on the write path: after deactivating a
// currency you could keep posting journals in it forever. The one dimension
// that DID enforce its flag was entry_templates.is_active, refused in
// core.EntryTemplate.Render -- this brings the other three to the same
// shape, at the one choke point every journal passes through.
//
// Existing balances and history are untouched: is_active is a soft delete
// precisely so journal_entries can keep its foreign keys. This gate is about
// NEW writes, the same way the period close line is.
func assertDimsActive(ctx context.Context, q *sqlcgen.Queries, journalTypeID int64, entries []resolvedEntry) error {
	currencyIDs := make([]int64, 0, len(entries))
	classIDs := make([]int64, 0, len(entries))
	seenCur := make(map[int64]struct{}, len(entries))
	seenCls := make(map[int64]struct{}, len(entries))
	for _, e := range entries {
		if _, ok := seenCur[e.currencyID]; !ok {
			seenCur[e.currencyID] = struct{}{}
			currencyIDs = append(currencyIDs, e.currencyID)
		}
		if _, ok := seenCls[e.classificationID]; !ok {
			seenCls[e.classificationID] = struct{}{}
			classIDs = append(classIDs, e.classificationID)
		}
	}

	rows, err := q.InactiveDims(ctx, sqlcgen.InactiveDimsParams{
		CurrencyIds:       currencyIDs,
		ClassificationIds: classIDs,
		JournalTypeIds:    []int64{journalTypeID},
	})
	if err != nil {
		return fmt.Errorf("postgres: dims: check active: %w", normalizeStoreError(err))
	}
	if len(rows) == 0 {
		return nil
	}

	refs := make([]string, len(rows))
	for i, r := range rows {
		refs[i] = fmt.Sprintf("%s %q", r.Kind, r.Code)
	}
	return fmt.Errorf(
		"postgres: %s is deactivated and cannot be used in a new journal: %w",
		strings.Join(refs, ", "), core.ErrInvalidInput,
	)
}
