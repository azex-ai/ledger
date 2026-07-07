package postgres

import (
	"context"
	"fmt"
	"strconv"

	"github.com/azex-ai/ledger/core"
	ledgerotel "github.com/azex-ai/ledger/pkg/otel"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
	"go.opentelemetry.io/otel/attribute"
)

var _ core.HolderReader = (*LedgerStore)(nil)

// Holder-scoped wallet read surface
// (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.3).

const (
	holderTxDefaultLimit = 20
	holderTxMaxLimit     = 100
)

// ListHolderBalances returns one HolderBalance per currency the holder has
// touched (or just the requested currency when currencyUID is non-empty).
// Each row reuses GetBalanceBreakdown's snapshot-consistent read; consistency
// is per currency, not across currencies.
func (s *LedgerStore) ListHolderBalances(ctx context.Context, holder int64, currencyUID string) ([]core.HolderBalance, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.holder.list_balances",
		attribute.Int64("account_holder", holder),
	)
	defer span.End()

	type cur struct{ uid, code string }
	var currencies []cur

	if currencyUID != "" {
		dim, err := s.dims.currencyByUIDOrErr(ctx, s.q, currencyUID)
		if err != nil {
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		currencies = []cur{{uid: currencyUID, code: dim.Code}}
	} else {
		rows, err := s.q.ListHolderCurrencies(ctx, holder)
		if err != nil {
			err = fmt.Errorf("postgres: list holder currencies: %w", err)
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		for _, r := range rows {
			currencies = append(currencies, cur{uid: pgToUID(r.Uid), code: r.Code})
		}
	}

	out := make([]core.HolderBalance, 0, len(currencies))
	for _, c := range currencies {
		b, err := s.GetBalanceBreakdown(ctx, holder, c.uid)
		if err != nil {
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		out = append(out, core.HolderBalance{
			CurrencyUID:  c.uid,
			CurrencyCode: c.code,
			Available:    b.Available,
			Pending:      b.Pending,
			Locked:       b.Locked,
			Total:        b.Total,
		})
	}
	return out, nil
}

// ListHolderTransactions returns the translated holder transaction view,
// newest first, cursor-paginated at journal granularity.
//
// The SQL projection returns one row per (journal, currency) net aggregate
// INCLUDING zero-net rows: those journals (holder-internal moves like
// available->locked) must still advance the cursor, so they are consumed
// here and only filtered from the output.
func (s *LedgerStore) ListHolderTransactions(ctx context.Context, holder int64, cursor string, limit int32) ([]core.HolderTransaction, string, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.holder.list_transactions",
		attribute.Int64("account_holder", holder),
	)
	defer span.End()

	if limit <= 0 {
		limit = holderTxDefaultLimit
	}
	if limit > holderTxMaxLimit {
		limit = holderTxMaxLimit
	}

	var cursorID int64
	if cursor != "" {
		id, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil || id <= 0 {
			err = fmt.Errorf("postgres: list holder transactions: bad cursor %q: %w", cursor, core.ErrInvalidInput)
			ledgerotel.RecordError(span, err)
			return nil, "", err
		}
		cursorID = id
	}

	rows, err := s.q.ListHolderTransactionRows(ctx, sqlcgen.ListHolderTransactionRowsParams{
		AccountHolder: holder,
		CursorID:      cursorID,
		PageLimit:     int64(limit),
	})
	if err != nil {
		err = fmt.Errorf("postgres: list holder transactions: %w", err)
		ledgerotel.RecordError(span, err)
		return nil, "", err
	}

	items := make([]core.HolderTransaction, 0, len(rows))
	journals := make(map[int64]struct{}, len(rows))
	minJournalID := int64(0)
	for _, r := range rows {
		journals[r.JournalID] = struct{}{}
		if minJournalID == 0 || r.JournalID < minJournalID {
			minJournalID = r.JournalID
		}
		net, err := numericToDecimal(r.NetAmount)
		if err != nil {
			err = fmt.Errorf("postgres: list holder transactions: journal %d: %w", r.JournalID, err)
			ledgerotel.RecordError(span, err)
			return nil, "", err
		}
		if net.IsZero() {
			// Holder-internal move (e.g. available->locked): invisible in the
			// user view; the holds surface expresses the locked state.
			continue
		}
		direction := core.HolderTransactionIn
		if net.IsNegative() {
			direction = core.HolderTransactionOut
		}
		items = append(items, core.HolderTransaction{
			UID:           pgToUID(r.JournalUid),
			Kind:          r.Kind,
			KindLabel:     r.KindLabel,
			Direction:     direction,
			Amount:        net.Abs(),
			CurrencyUID:   pgToUID(r.CurrencyUid),
			CurrencyCode:  r.CurrencyCode,
			OccurredAt:    r.EffectiveAt,
			ReversalOfUID: r.ReversalOfUid,
			Memo:          r.Memo,
		})
	}

	// More pages exist iff this page consumed a full window of journals.
	nextCursor := ""
	if int32(len(journals)) == limit && minJournalID > 0 {
		nextCursor = strconv.FormatInt(minJournalID, 10)
	}
	return items, nextCursor, nil
}

// ListHolderHolds returns the holder's outstanding reservation holds
// (active = full reserved amount, settling = unsettled remainder — the same
// figures the balance breakdown counts as locked).
func (s *LedgerStore) ListHolderHolds(ctx context.Context, holder int64) ([]core.HolderHold, error) {
	ctx, span := ledgerotel.StartSpan(ctx, "ledger.holder.list_holds",
		attribute.Int64("account_holder", holder),
	)
	defer span.End()

	rows, err := s.q.ListHolderHolds(ctx, holder)
	if err != nil {
		err = fmt.Errorf("postgres: list holder holds: %w", err)
		ledgerotel.RecordError(span, err)
		return nil, err
	}
	out := make([]core.HolderHold, 0, len(rows))
	for _, r := range rows {
		amount, err := numericToDecimal(r.HeldAmount)
		if err != nil {
			err = fmt.Errorf("postgres: list holder holds: reservation %s: %w", pgToUID(r.Uid), err)
			ledgerotel.RecordError(span, err)
			return nil, err
		}
		out = append(out, core.HolderHold{
			UID:          pgToUID(r.Uid),
			Amount:       amount,
			CurrencyUID:  pgToUID(r.CurrencyUid),
			CurrencyCode: r.CurrencyCode,
			CreatedAt:    r.CreatedAt,
			ExpiresAt:    r.ExpiresAt,
		})
	}
	return out, nil
}
