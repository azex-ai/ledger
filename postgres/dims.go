// Package postgres — dims.go
// In-process id<->uid resolution for the three config dimensions (currencies,
// classifications, journal types). uid is the only identifier in any public
// contract (api-contract §3); internal BIGSERIAL ids are storage details, so
// every store boundary crossing resolves through here.
//
// Cache safety: config rows are insert-only for the cached fields (id, uid,
// code, normal_side, exponent are all immutable after creation; only
// is_active mutates, and it is deliberately NOT cached). A miss triggers one
// full-table refresh — these tables are small by design.
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

// lookup runs get under the read lock; on a miss it refreshes once and
// retries. The second miss is the caller's ErrNotFound.
func dimLookup[K comparable, V any](ctx context.Context, c *dimCache, q *sqlcgen.Queries, pick func() map[K]V, key K) (V, bool, error) {
	c.mu.RLock()
	m := pick()
	v, ok := m[key]
	c.mu.RUnlock()
	if ok {
		return v, true, nil
	}
	if err := c.refresh(ctx, q); err != nil {
		var zero V
		return zero, false, err
	}
	c.mu.RLock()
	m = pick()
	v, ok = m[key]
	c.mu.RUnlock()
	return v, ok, nil
}

func (c *dimCache) currencyByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimCurrency, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[string]dimCurrency { return c.currencyByUID }, uid)
	if err != nil {
		return dimCurrency{}, err
	}
	if !ok {
		return dimCurrency{}, fmt.Errorf("postgres: currency %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) currencyByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimCurrency, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[int64]dimCurrency { return c.currencyByID }, id)
	if err != nil {
		return dimCurrency{}, err
	}
	if !ok {
		return dimCurrency{}, fmt.Errorf("postgres: currency id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) classByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimClassification, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[string]dimClassification { return c.classByUID }, uid)
	if err != nil {
		return dimClassification{}, err
	}
	if !ok {
		return dimClassification{}, fmt.Errorf("postgres: classification %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) classByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimClassification, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[int64]dimClassification { return c.classByID }, id)
	if err != nil {
		return dimClassification{}, err
	}
	if !ok {
		return dimClassification{}, fmt.Errorf("postgres: classification id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) jtByUIDOrErr(ctx context.Context, q *sqlcgen.Queries, uid string) (dimJournalType, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[string]dimJournalType { return c.jtByUID }, uid)
	if err != nil {
		return dimJournalType{}, err
	}
	if !ok {
		return dimJournalType{}, fmt.Errorf("postgres: journal type %q: %w", uid, core.ErrNotFound)
	}
	return v, nil
}

func (c *dimCache) jtByIDOrErr(ctx context.Context, q *sqlcgen.Queries, id int64) (dimJournalType, error) {
	v, ok, err := dimLookup(ctx, c, q, func() map[int64]dimJournalType { return c.jtByID }, id)
	if err != nil {
		return dimJournalType{}, err
	}
	if !ok {
		return dimJournalType{}, fmt.Errorf("postgres: journal type id %d: %w", id, core.ErrNotFound)
	}
	return v, nil
}
