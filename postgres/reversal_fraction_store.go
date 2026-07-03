package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/azex-ai/ledger/core"
	"github.com/azex-ai/ledger/postgres/sqlcgen"
)

// reversalScaleGuardDigits is extra decimal precision kept when computing
// total*num/den before rounding down to the currency's exponent. Fractions
// like 1/3 are not exactly representable in decimal; 12 guard digits beyond
// the target exponent is generous for amounts bounded by NUMERIC(30,18) and
// ensures the guard digits themselves never influence the final rounding.
const reversalScaleGuardDigits int32 = 12

// entryDimKey identifies an account dimension + entry direction within a
// journal, used to track how much of an original entry has already been
// reversed across (possibly several) partial reversals. Reversal entries
// carry no FK back to the specific original entry row they correspond to, so
// this dimension key is the finest grain available; it is exact as long as a
// journal does not post two entries on the same dimension with the same
// entry_type (true of every preset and every journal built via templates —
// PostJournal does not itself forbid it, so this is a documented assumption,
// not an enforced one).
type entryDimKey struct {
	holder           int64
	currencyID       int64
	classificationID int64
	entryType        core.EntryType
}

// ReverseJournalFraction posts a reversal covering num/den of the journal's
// entries. See core.JournalWriter's doc comment for the full contract.
//
// In pool mode a new transaction is started and committed here (the
// SELECT...FOR UPDATE row lock and the resulting insert must share one
// transaction so the lock covers both the cumulative-amount check and the
// write). In tx mode (store bound via WithDB) it participates in the
// caller's transaction; commit/rollback is the caller's responsibility.
func (s *LedgerStore) ReverseJournalFraction(ctx context.Context, journalUID string, num, den int64, reason string, idempotencyKey string) (*core.Journal, error) {
	if err := core.ValidateReversalFraction(num, den); err != nil {
		return nil, err
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("postgres: reverse journal fraction: idempotency key required: %w", core.ErrInvalidInput)
	}

	if s.pool == nil {
		return s.reverseJournalFractionWithQueries(ctx, s.q, journalUID, num, den, reason, idempotencyKey)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.reverseJournalFractionWithQueries(ctx, qtx, journalUID, num, den, reason, idempotencyKey)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: commit: %w", err)
	}
	return journal, nil
}

func (s *LedgerStore) reverseJournalFractionWithQueries(ctx context.Context, q *sqlcgen.Queries, journalUID string, num, den int64, reason, idempotencyKey string) (*core.Journal, error) {
	expectedFraction := fmt.Sprintf("%d/%d", num, den)

	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return nil, err
	}

	// Idempotent replay short-circuit. This must happen before the row lock
	// below (and before any cumulative-amount computation): a retried call
	// with the same key would otherwise see its own, already-committed
	// entries as "reversed by someone else" via ListReversalEntriesByOriginal
	// and reject itself.
	// Row-lock the original journal for the rest of this transaction so
	// concurrent partial reversals of it serialize. Without this, two
	// concurrent calls could each read "0 reversed so far" and both post,
	// together over-reversing the journal beyond its original amount.
	original, err := q.GetJournalForUpdateByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: reverse journal fraction: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return nil, fmt.Errorf("postgres: reverse journal fraction: get journal: %w", err)
	}
	journalID := original.ID

	if existing, err := q.GetJournalByIdempotencyKey(ctx, idempotencyKey); err == nil {
		if !existing.ReversalOf.Valid || existing.ReversalOf.Int64 != journalID {
			return nil, fmt.Errorf("postgres: reverse journal fraction: idempotency key %q already used for a different journal: %w", idempotencyKey, core.ErrConflict)
		}
		existingMeta := jsonToMetadata(existing.Metadata)
		if existingMeta["reason"] != reason || existingMeta["reversal_fraction"] != expectedFraction {
			return nil, fmt.Errorf("postgres: reverse journal fraction: idempotency key %q payload mismatch: %w", idempotencyKey, core.ErrConflict)
		}
		return journalFromRow(ctx, s.dims, q, existing)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("postgres: reverse journal fraction: check idempotency: %w", err)
	}

	if original.ReversalOf.Valid {
		return nil, fmt.Errorf("postgres: reverse journal fraction: journal %q is already a reversal: %w", journalUID, core.ErrConflict)
	}

	entries, err := q.ListJournalEntries(ctx, journalID)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: list entries: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("postgres: reverse journal fraction: journal %d has no entries: %w", journalID, core.ErrNotFound)
	}

	alreadyReversed, err := cumulativeReversedByDimension(ctx, q, journalID)
	if err != nil {
		return nil, err
	}

	// num == den is the "reverse everything remaining" form: each entry's
	// reversal amount is exactly its original amount minus what prior
	// reversals already covered. This is the only way to complete a reversal
	// whose earlier fractional steps rounded up (e.g. two 1/3 reversals of
	// 100.01 at exponent 2 reverse 33.34 + 33.34; a third 1/3 would round to
	// 33.34 again and overshoot — the exact remainder 33.33 is not expressible
	// as any small fraction of the original). Balance safety: the original
	// journal balances per currency and every prior reversal balanced per
	// currency, so the per-currency remainder is equal on the debit and
	// credit sides by subtraction.
	if num == den {
		reversedEntries := make([]core.EntryInput, 0, len(entries))
		for _, e := range entries {
			originalType := core.EntryType(e.EntryType)
			key := entryDimKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID, entryType: originalType}
			remaining := mustNumericToDecimal(e.Amount).Sub(alreadyReversed[key])
			if !remaining.IsPositive() {
				continue
			}
			flipped := core.EntryTypeCredit
			if originalType == core.EntryTypeCredit {
				flipped = core.EntryTypeDebit
			}
			cur, err := s.dims.currencyByIDOrErr(ctx, q, e.CurrencyID)
			if err != nil {
				return nil, err
			}
			cls, err := s.dims.classByIDOrErr(ctx, q, e.ClassificationID)
			if err != nil {
				return nil, err
			}
			reversedEntries = append(reversedEntries, core.EntryInput{
				AccountHolder:     e.AccountHolder,
				CurrencyUID:       cur.UID,
				ClassificationUID: cls.UID,
				EntryType:         flipped,
				Amount:            remaining,
			})
		}
		if len(reversedEntries) == 0 {
			return nil, fmt.Errorf("postgres: reverse journal fraction: journal %d is already fully reversed: %w", journalID, core.ErrConflict)
		}
		jt, err := s.dims.jtByIDOrErr(ctx, q, original.JournalTypeID)
		if err != nil {
			return nil, err
		}
		input := core.JournalInput{
			JournalTypeUID: jt.UID,
			IdempotencyKey: idempotencyKey,
			Entries:        reversedEntries,
			Source:         "reversal",
			ReversalOfUID:  journalUID,
			Metadata:       map[string]string{"reason": reason, "reversal_fraction": expectedFraction},
		}
		return s.postJournalWithQueries(ctx, q, input)
	}

	// Group original entries by (currency, entry_type) so each group's total
	// is scaled by num/den once and split back across the group's entries via
	// Allocate — this guarantees the reversal journal is itself per-currency
	// balanced: the original journal already balances per currency (debit
	// total == credit total, enforced by JournalInput.Validate), and applying
	// the exact same deterministic scale-and-round to two equal decimal
	// values yields two equal results.
	type groupKey struct {
		currencyID int64
		entryType  core.EntryType
	}
	groups := make(map[groupKey][]int, len(entries))
	for i, e := range entries {
		gk := groupKey{currencyID: e.CurrencyID, entryType: core.EntryType(e.EntryType)}
		groups[gk] = append(groups[gk], i)
	}

	reversedAmounts := make([]decimal.Decimal, len(entries))
	for gk, idxs := range groups {
		currency, err := s.dims.currencyByIDOrErr(ctx, q, gk.currencyID)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal fraction: %w", err)
		}
		exponent := currency.Exponent

		groupTotal := decimal.Zero
		weights := make([]decimal.Decimal, len(idxs))
		for wi, idx := range idxs {
			amt := mustNumericToDecimal(entries[idx].Amount)
			groupTotal = groupTotal.Add(amt)
			weights[wi] = amt
		}

		scaledTotal := scaleByFraction(groupTotal, num, den, exponent)

		shares, err := core.Allocate(scaledTotal, weights, exponent)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal fraction: allocate currency %d %s: %w", gk.currencyID, gk.entryType, err)
		}
		for wi, idx := range idxs {
			reversedAmounts[idx] = shares[wi]
		}
	}

	reversedEntries := make([]core.EntryInput, 0, len(entries))
	for i, e := range entries {
		newAmount := reversedAmounts[i]
		if !newAmount.IsPositive() {
			// This entry's share rounded to zero at the currency's exponent
			// (possible for a very small fraction of a very small amount).
			// Posting a zero-amount entry is meaningless and JournalInput
			// rejects it outright, so it is simply omitted from the reversal.
			continue
		}

		originalAmount := mustNumericToDecimal(e.Amount)
		originalType := core.EntryType(e.EntryType)
		key := entryDimKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID, entryType: originalType}
		already := alreadyReversed[key]
		if already.Add(newAmount).GreaterThan(originalAmount) {
			var entryID int64
			if e.ID.Valid {
				entryID = e.ID.Int64
			}
			return nil, fmt.Errorf(
				"postgres: reverse journal fraction: entry %d: cumulative reversed %s + this reversal's %s would exceed original amount %s: %w",
				entryID, already, newAmount, originalAmount, core.ErrConflict,
			)
		}

		flipped := core.EntryTypeCredit
		if originalType == core.EntryTypeCredit {
			flipped = core.EntryTypeDebit
		}
		cur, err := s.dims.currencyByIDOrErr(ctx, q, e.CurrencyID)
		if err != nil {
			return nil, err
		}
		cls, err := s.dims.classByIDOrErr(ctx, q, e.ClassificationID)
		if err != nil {
			return nil, err
		}
		reversedEntries = append(reversedEntries, core.EntryInput{
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       cur.UID,
			ClassificationUID: cls.UID,
			EntryType:         flipped,
			Amount:            newAmount,
		})
	}
	if len(reversedEntries) == 0 {
		return nil, fmt.Errorf("postgres: reverse journal fraction: fraction %d/%d of journal %s rounds to zero on every entry: %w", num, den, journalUID, core.ErrInvalidInput)
	}

	jt, err := s.dims.jtByIDOrErr(ctx, q, original.JournalTypeID)
	if err != nil {
		return nil, err
	}
	input := core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: idempotencyKey,
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOfUID:  journalUID,
		Metadata:       map[string]string{"reason": reason, "reversal_fraction": expectedFraction},
	}

	return s.postJournalWithQueries(ctx, q, input)
}

// cumulativeReversedByDimension sums, per account dimension and *original*
// entry_type, the amount already reversed across every prior reversal (full
// or partial) of journalID.
func cumulativeReversedByDimension(ctx context.Context, q *sqlcgen.Queries, journalID int64) (map[entryDimKey]decimal.Decimal, error) {
	rows, err := q.ListReversalEntriesByOriginal(ctx, int64ToInt8(&journalID))
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: list existing reversal entries: %w", err)
	}
	out := make(map[entryDimKey]decimal.Decimal, len(rows))
	for _, r := range rows {
		// Reversal entries are flipped relative to the original; invert back
		// to the original entry_type to key the same dimension.
		originalType := core.EntryTypeCredit
		if core.EntryType(r.EntryType) == core.EntryTypeCredit {
			originalType = core.EntryTypeDebit
		}
		key := entryDimKey{holder: r.AccountHolder, currencyID: r.CurrencyID, classificationID: r.ClassificationID, entryType: originalType}
		out[key] = out[key].Add(mustNumericToDecimal(r.Amount))
	}
	return out, nil
}

// scaleByFraction computes total*num/den rounded to exponent decimal places
// using core.RoundHalfUp. The intermediate DivRound keeps reversalScaleGuardDigits
// of extra precision so the final rounding is accurate regardless of num/den
// (e.g. 1/3 is not exactly representable in decimal).
func scaleByFraction(total decimal.Decimal, num, den int64, exponent int32) decimal.Decimal {
	raw := total.Mul(decimal.NewFromInt(num)).DivRound(decimal.NewFromInt(den), exponent+reversalScaleGuardDigits)
	return core.Round(raw, exponent, core.RoundHalfUp)
}
