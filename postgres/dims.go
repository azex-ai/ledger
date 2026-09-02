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
// The shared cache is therefore refreshed through the POOL, never through
// the caller's Queries, so it can only ever contain committed rows. A uid
// that is still missing after that refresh is resolved once through the
// caller's own Queries into a throwaway cache — so a row created earlier in
// the same open transaction still resolves, without being published. That
// second read only happens on a genuine miss (a not-found error, or an
// in-transaction creation), never on the hot path.
//
// A miss triggers one full-table refresh — these tables are small by design.
package postgres

import (
	"context"
	"fmt"
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

	// pool is the connection pool this cache belongs to, or nil for a
	// throwaway cache (see dimLookup's overlay). Non-nil means "refresh
	// through the pool, ignoring whatever Queries the caller handed in" —
	// that is what keeps uncommitted rows out of process-wide state.
	pool *pgxpool.Pool

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
	c, _ := dimCaches.LoadOrStore(pool, &dimCache{pool: pool})
	return c.(*dimCache)
}

// refresh reloads all three dimension tables.
//
// When this cache belongs to a pool (the shared, process-wide case) the read
// goes through the pool and q is deliberately ignored: q may be a caller's
// open transaction, and publishing its uncommitted rows into shared state is
// the B-m5 bug (see the package doc). q is used only by the throwaway
// overlay cache in dimLookup, whose contents are discarded.
func (c *dimCache) refresh(ctx context.Context, q *sqlcgen.Queries) error {
	if c.pool != nil {
		q = sqlcgen.New(c.pool)
	}
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

// dimLookup runs pick under the read lock; on a miss it refreshes the shared
// cache once (through the pool — committed rows only) and retries. If the key
// is STILL missing, it resolves once through the caller's own Queries into a
// throwaway cache and returns that result WITHOUT caching it: a config row
// created earlier in the caller's still-open transaction has to resolve, but
// it must not become visible to the rest of the process before it commits
// (see the package doc, dimension 2). A miss on that read is the caller's
// ErrNotFound.
//
// pick takes the cache to read from precisely so the overlay can be
// consulted with the same accessor as the shared cache.
func dimLookup[K comparable, V any](ctx context.Context, c *dimCache, q *sqlcgen.Queries, pick func(*dimCache) map[K]V, key K) (V, bool, error) {
	c.mu.RLock()
	v, ok := pick(c)[key]
	c.mu.RUnlock()
	if ok {
		return v, true, nil
	}
	if err := c.refresh(ctx, q); err != nil {
		var zero V
		return zero, false, err
	}
	c.mu.RLock()
	v, ok = pick(c)[key]
	c.mu.RUnlock()
	if ok || c.pool == nil {
		// c.pool == nil means c IS a throwaway cache (or a store built
		// without a pool): refresh already read through q, so there is
		// nothing further to try.
		return v, ok, nil
	}

	overlay := &dimCache{}
	if err := overlay.refresh(ctx, q); err != nil {
		var zero V
		return zero, false, err
	}
	v, ok = pick(overlay)[key]
	return v, ok, nil
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
