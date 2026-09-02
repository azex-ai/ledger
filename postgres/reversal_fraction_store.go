package postgres

import (
	"bytes"
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
//
// Pool mode with an Attestor configured additionally pre-authorizes the
// reversal via AuthorizeReversal, strictly before pool.Begin (board #15,
// W2-T1 -- closing the same signing gap design doc §7.5 closed for
// PostJournal, extended here to cover reversals). See
// reverseJournalFractionWithQueries's doc comment for how the pre-computed
// signature is validated against what the row lock actually finds.
func (s *LedgerStore) ReverseJournalFraction(ctx context.Context, journalUID string, num, den int64, reason string, idempotencyKey string) (*core.Journal, error) {
	if err := core.ValidateReversalFraction(num, den); err != nil {
		return nil, err
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("postgres: reverse journal fraction: idempotency key required: %w", core.ErrInvalidInput)
	}

	if s.pool == nil {
		return s.reverseJournalFractionWithQueries(ctx, s.q, journalUID, num, den, reason, idempotencyKey, nil, journalAuth{status: core.AuthStatusUnsignedTxMode})
	}

	var preAuth *core.AuthorizedJournal
	fallback := journalAuth{status: core.AuthStatusUnsignedNoAttestor}
	if s.attestor != nil {
		authorized, err := s.AuthorizeReversal(ctx, journalUID, num, den, reason, idempotencyKey)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal fraction: %w", err)
		}
		preAuth = &authorized
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := s.q.WithTx(tx)
	journal, err := s.reverseJournalFractionWithQueries(ctx, qtx, journalUID, num, den, reason, idempotencyKey, preAuth, fallback)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("postgres: reverse journal fraction: commit: %w", err)
	}
	return journal, nil
}

// reverseJournalFractionWithQueries does the actual read-lock-write. preAuth,
// when non-nil, is the result of a prior AuthorizeReversal call made outside
// this transaction (ReverseJournalFraction's pool-mode branch, above): after
// this function re-derives the reversal's entries fresh, under the original
// journal's row lock (the only place that data can be trusted), it recomputes
// the canonical digest and compares it to preAuth.Digest. A match means
// nothing relevant changed between authorization and posting, so preAuth's
// signature is used verbatim (auth_status = signed). A mismatch means a
// concurrent partial reversal landed in between and changed what "reverse
// num/den" actually resolves to for the num==den ("reverse everything
// remaining") form -- the ONLY form whose entries depend on mutable state
// (see reversalEntriesFor's doc comment) -- and the post is rejected
// (core.ErrConflict) rather than silently using a stale signature, silently
// falling back to unsigned, or calling the Attestor again from inside this
// transaction (financial.md forbids that regardless). fallback is the
// journalAuth used when preAuth is nil (no Attestor configured in pool mode,
// or this call is running in tx mode where there was never a safe point to
// call AuthorizeReversal at all -- see ReverseJournalFraction's doc comment).
func (s *LedgerStore) reverseJournalFractionWithQueries(ctx context.Context, q *sqlcgen.Queries, journalUID string, num, den int64, reason, idempotencyKey string, preAuth *core.AuthorizedJournal, fallback journalAuth) (*core.Journal, error) {
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
		existingMeta, metaErr := jsonToMetadata(existing.Metadata)
		if metaErr != nil {
			// An unparseable stored blob cannot be compared, and treating it
			// as absent used to make a DIFFERENT payload compare equal
			// (operability I-23). Fail closed on the conflict side.
			return nil, fmt.Errorf("postgres: reverse journal fraction: idempotency key %q: stored metadata unreadable: %w: %w", idempotencyKey, metaErr, core.ErrConflict)
		}
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

	// reversalEntriesFor holds the num==den ("reverse everything remaining")
	// vs general-fraction branching (see its doc comment); this is the same
	// derivation AuthorizeReversal ran, unlocked, before this transaction
	// opened -- see the digest comparison below.
	reversedEntries, err := s.reversalEntriesFor(ctx, q, entries, alreadyReversed, num, den)
	if err != nil {
		return nil, err
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

	// Default: no pre-authorization to honor (fallback, set by the caller --
	// either unsigned_no_attestor in pool mode with no Attestor configured,
	// or unsigned_tx_mode when this call has no safe point to have called
	// AuthorizeReversal at all, i.e. tx mode). resolveEffectiveAt picks "now"
	// exactly like PostJournal's tx-mode branch always has.
	effectiveAt := resolveEffectiveAt(input.EffectiveAt)
	auth := fallback
	if preAuth != nil {
		// Reuse the EXACT instant AuthorizeReversal signed, never a fresh
		// "now" -- CanonicalJournalDigest covers effectiveAt, so resolving a
		// second, different "now" here would make every comparison below
		// fail even with zero concurrent state change (same reasoning as
		// PostAuthorized reusing AuthorizedJournal.EffectiveAt instead of
		// calling resolveEffectiveAt again).
		effectiveAt = preAuth.EffectiveAt
		digest, err := core.CanonicalJournalDigest(input, effectiveAt)
		if err != nil {
			return nil, fmt.Errorf("postgres: reverse journal fraction: recompute digest: %w", err)
		}
		if !bytes.Equal(digest, preAuth.Digest) {
			return nil, fmt.Errorf(
				"postgres: reverse journal fraction: reversal intent changed since AuthorizeReversal ran (a concurrent reversal of journal %q likely landed in between); retry: %w",
				journalUID, core.ErrConflict,
			)
		}
		auth = journalAuth{digest: preAuth.Digest, signature: preAuth.Signature, keyID: preAuth.KeyID, status: preAuth.Status}
	}
	return s.postJournalWithQueries(ctx, q, input, effectiveAt, auth)
}

// reversalEntriesFor derives the entries a reversal covering num/den of
// original's entries (rows in entries) would post, given alreadyReversed
// (cumulative amount already reversed per account dimension, from
// cumulativeReversedByDimension). It has no DB writes and no side effects
// beyond read-only dimension lookups, so it produces byte-identical output
// whenever entries/alreadyReversed are byte-identical -- the property that
// lets AuthorizeReversal (unlocked, outside any transaction) and
// reverseJournalFractionWithQueries (under the original journal's row lock)
// safely share it: if reversal history has not changed between the two
// calls, they get the same []core.EntryInput and therefore the same
// core.CanonicalJournalDigest, so a signature obtained outside the
// transaction is still valid once the lock is taken.
//
// Only the num == den branch ("reverse everything remaining": each entry's
// amount is its original minus what prior reversals already covered) reads
// alreadyReversed at all -- the general proportional-split branch (num !=
// den) computes each entry's share purely from the ORIGINAL journal's own
// entries, so its output can never differ between an unlocked call and a
// locked one; the overshoot check there rejects the whole post if
// alreadyReversed grew too large in between, but it does not change what
// entries would have been derived. This is why a concurrent partial
// reversal landing between AuthorizeReversal and the eventual post can only
// ever invalidate a num==den authorization, never a fractional one -- see
// reverseJournalFractionWithQueries's digest-comparison guard for what
// happens when it does.
func (s *LedgerStore) reversalEntriesFor(ctx context.Context, q *sqlcgen.Queries, entries []sqlcgen.ListJournalEntriesRow, alreadyReversed map[entryDimKey]decimal.Decimal, num, den int64) ([]core.EntryInput, error) {
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
	//
	// The remainder is computed per DIMENSION, not per entry, because that is
	// the granularity alreadyReversed is keyed at. A journal may carry more
	// than one entry on the same (holder, currency, classification,
	// entry_type) -- JournalInput.Validate checks per-currency balance and
	// does not deduplicate -- and subtracting a dimension-wide total from each
	// individual entry charges the same prior reversal once per entry.
	//
	// Concretely, on a journal of 60 + 40 debits sharing a dimension, reversed
	// half and then "all the rest": each entry subtracted the full 50 already
	// reversed, giving 10 and -10; the negative was skipped as non-positive
	// and the result was a balanced 10/10 reversal that Validate accepted. 40
	// stayed on the books and the caller was told the journal was fully
	// reversed. Aggregating first makes the arithmetic match the bookkeeping.
	if num == den {
		// Sum the original amounts per dimension, keeping first-appearance
		// order so the emitted entries are deterministic across calls -- the
		// digest guard in reverseJournalFractionWithQueries compares derived
		// entries between an unlocked and a locked read, and map iteration
		// order would make an unchanged journal look changed.
		originalByDim := make(map[entryDimKey]decimal.Decimal, len(entries))
		representative := make(map[entryDimKey]sqlcgen.ListJournalEntriesRow, len(entries))
		order := make([]entryDimKey, 0, len(entries))
		for _, e := range entries {
			key := entryDimKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID, entryType: core.EntryType(e.EntryType)}
			if _, seen := originalByDim[key]; !seen {
				order = append(order, key)
				representative[key] = e
			}
			originalByDim[key] = originalByDim[key].Add(mustNumericToDecimal(e.Amount))
		}

		reversedEntries := make([]core.EntryInput, 0, len(order))
		for _, key := range order {
			remaining := originalByDim[key].Sub(alreadyReversed[key])
			if !remaining.IsPositive() {
				continue
			}
			e := representative[key]
			flipped := core.EntryTypeCredit
			if key.entryType == core.EntryTypeCredit {
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
			return nil, fmt.Errorf("postgres: reverse journal fraction: journal is already fully reversed: %w", core.ErrConflict)
		}
		return reversedEntries, nil
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

	// Overshoot check. alreadyReversed is keyed per DIMENSION, so both sides
	// of the comparison must be aggregated to that same grain: this reversal's
	// shares and the original amounts. Comparing the dimension-wide cumulative
	// against a single entry's original amount rejected legal reversals on
	// journals carrying two entries on one dimension (60 + 40 debits, reversed
	// half then a second legal half: already=50 + share 30 > 60 — a phantom
	// excess). Same aggregation, same reason as the num==den branch above (C8);
	// this check only rejects, it never changes the derived entries, so the
	// digest determinism AuthorizeReversal relies on is untouched.
	newByDim := make(map[entryDimKey]decimal.Decimal, len(entries))
	originalByDim := make(map[entryDimKey]decimal.Decimal, len(entries))
	dimOrder := make([]entryDimKey, 0, len(entries))
	for i, e := range entries {
		key := entryDimKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID, entryType: core.EntryType(e.EntryType)}
		if _, seen := originalByDim[key]; !seen {
			dimOrder = append(dimOrder, key)
		}
		originalByDim[key] = originalByDim[key].Add(mustNumericToDecimal(e.Amount))
		if reversedAmounts[i].IsPositive() {
			newByDim[key] = newByDim[key].Add(reversedAmounts[i])
		}
	}
	for _, key := range dimOrder {
		already := alreadyReversed[key]
		if already.Add(newByDim[key]).GreaterThan(originalByDim[key]) {
			return nil, fmt.Errorf(
				"postgres: reverse journal fraction: dimension (holder %d, currency %d, classification %d, %s): cumulative reversed %s + this reversal's %s would exceed original amount %s: %w",
				key.holder, key.currencyID, key.classificationID, key.entryType, already, newByDim[key], originalByDim[key], core.ErrConflict,
			)
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

		originalType := core.EntryType(e.EntryType)
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
		return nil, fmt.Errorf("postgres: reverse journal fraction: fraction %d/%d rounds to zero on every entry: %w", num, den, core.ErrInvalidInput)
	}
	return reversedEntries, nil
}

// AuthorizeReversal computes the canonical digest of the reversal
// ReverseJournal (num=1, den=1) or ReverseJournalFraction (any valid
// num/den) would post, and signs it if an Attestor is configured -- entirely
// outside any database transaction (design doc §7.2/§7.5, extended to cover
// reversals by board #15, W2-T1). See core.JournalWriter's doc comment for
// the full contract and the caveats about what a caller may safely do with
// the result.
//
// Unlike Authorize (which signs a caller-supplied core.JournalInput
// verbatim), a reversal's entries are DERIVED from the original journal --
// this method reads it, its entries, and its prior reversal history via an
// ordinary (unlocked) query, uses exactly the same derivation
// (reversalEntriesFor) reverseJournalFractionWithQueries uses under the row
// lock, then hands the resulting core.JournalInput to Authorize. That is
// what makes the digest comparison at post time meaningful: same inputs,
// same function, same output.
func (s *LedgerStore) AuthorizeReversal(ctx context.Context, journalUID string, num, den int64, reason string, idempotencyKey string) (core.AuthorizedJournal, error) {
	if s.pool == nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: called on a transaction-bound store; AuthorizeReversal must run before opening a transaction, not from inside RunInTx: %w", core.ErrInvalidInput)
	}
	if err := core.ValidateReversalFraction(num, den); err != nil {
		return core.AuthorizedJournal{}, err
	}
	if idempotencyKey == "" {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: idempotency key required: %w", core.ErrInvalidInput)
	}

	pgUID, err := uidToPG(journalUID)
	if err != nil {
		return core.AuthorizedJournal{}, err
	}
	// Unlocked read: this runs outside any transaction, so there is no row
	// lock to take. The row lock that actually protects against a
	// concurrent partial reversal is taken later, inside
	// reverseJournalFractionWithQueries -- this call only produces a
	// best-effort intent to sign, which that lock-protected code
	// re-validates (via the digest comparison) before trusting it.
	original, err := s.q.GetJournalByUID(ctx, pgUID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: journal %q: %w", journalUID, core.ErrNotFound)
		}
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: get journal: %w", err)
	}
	if original.ReversalOf.Valid {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: journal %q is already a reversal: %w", journalUID, core.ErrConflict)
	}

	entries, err := s.q.ListJournalEntries(ctx, original.ID)
	if err != nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: list entries: %w", err)
	}
	if len(entries) == 0 {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: journal %q has no entries: %w", journalUID, core.ErrNotFound)
	}

	alreadyReversed, err := cumulativeReversedByDimension(ctx, s.q, original.ID)
	if err != nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: %w", err)
	}

	reversedEntries, err := s.reversalEntriesFor(ctx, s.q, entries, alreadyReversed, num, den)
	if err != nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: %w", err)
	}

	jt, err := s.dims.jtByIDOrErr(ctx, s.q, original.JournalTypeID)
	if err != nil {
		return core.AuthorizedJournal{}, fmt.Errorf("postgres: authorize reversal: %w", err)
	}

	input := core.JournalInput{
		JournalTypeUID: jt.UID,
		IdempotencyKey: idempotencyKey,
		Entries:        reversedEntries,
		Source:         "reversal",
		ReversalOfUID:  journalUID,
		Metadata:       map[string]string{"reason": reason, "reversal_fraction": fmt.Sprintf("%d/%d", num, den)},
	}

	return s.Authorize(ctx, input)
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

// flipEntryType returns the opposite side of a double entry. A reversal entry
// is the original entry with its side flipped, so this is also how a reversal
// entry is mapped back onto the dimension of the original it reverses.
func flipEntryType(t core.EntryType) core.EntryType {
	if t == core.EntryTypeCredit {
		return core.EntryTypeDebit
	}
	return core.EntryTypeCredit
}

// validateReversalOfInput is the input gate on the caller-supplied
// core.JournalInput.ReversalOfUID (I-51). ReverseJournal and
// ReverseJournalFraction DERIVE a reversal's entries from the original and
// guard both ends of the chain; PostJournal accepts the same link from a
// caller and, until this gate existed, checked only that the uid resolved to
// a row. That was enough to break the chain's integrity without breaking
// double entry: everything downstream -- cumulativeReversedByDimension above
// all -- reads every journal carrying reversal_of = J as "a reversal of J
// worth this much", so a journal that moves no money at all can register as
// reversal history and make "reverse everything remaining" reverse less than
// everything, with a nil error and every reconciliation check green
// (financial-correctness.md A-Critical-2, the 2026-08-26 C8 defect reaching
// the same code through an unguarded input instead of a bad derivation).
//
// The three rules mirror what the reversal APIs already enforce, so the two
// ways to post a reversal cannot disagree about the same journal:
//
//  1. the referenced journal must not itself be a reversal (ErrConflict --
//     same verdict as ledger_store.go's ReverseJournal and
//     reverseJournalFractionWithQueries);
//  2. every entry must invert an entry the original actually has, on the same
//     (holder, currency, classification) dimension (ErrInvalidInput) -- this
//     is what a "reversal" means, and it is the rule the net-zero repro
//     violates;
//  3. per dimension, already-reversed + this journal's amount must not exceed
//     the original's (ErrConflict -- the same ceiling, aggregated at the same
//     grain, as reversalEntriesFor's overshoot check).
//
// original must have been read FOR UPDATE by the caller: rules 2 and 3 read
// the original's entries and its reversal history, and both have to stay put
// until this journal commits.
func validateReversalOfInput(ctx context.Context, q *sqlcgen.Queries, original sqlcgen.Journal, resolved []resolvedEntry) error {
	originalUID := pgToUID(original.Uid)
	if original.ReversalOf.Valid {
		return fmt.Errorf(
			"postgres: post journal: reversal_of %q is itself a reversal; reverse the original journal instead: %w",
			originalUID, core.ErrConflict,
		)
	}

	originalEntries, err := q.ListJournalEntries(ctx, original.ID)
	if err != nil {
		return fmt.Errorf("postgres: post journal: reversal_of: list original entries: %w", err)
	}
	if len(originalEntries) == 0 {
		return fmt.Errorf(
			"postgres: post journal: reversal_of %q has no entries; there is nothing to reverse: %w",
			originalUID, core.ErrInvalidInput,
		)
	}

	originalByDim := make(map[entryDimKey]decimal.Decimal, len(originalEntries))
	for _, e := range originalEntries {
		key := entryDimKey{holder: e.AccountHolder, currencyID: e.CurrencyID, classificationID: e.ClassificationID, entryType: core.EntryType(e.EntryType)}
		originalByDim[key] = originalByDim[key].Add(mustNumericToDecimal(e.Amount))
	}

	alreadyReversed, err := cumulativeReversedByDimension(ctx, q, original.ID)
	if err != nil {
		return fmt.Errorf("postgres: post journal: reversal_of: %w", err)
	}

	// Aggregate this journal's entries onto the ORIGINAL's dimension grain
	// (each entry's side flipped back), keeping first-appearance order so the
	// error a caller gets names the first offending leg deterministically.
	newByDim := make(map[entryDimKey]decimal.Decimal, len(resolved))
	order := make([]entryDimKey, 0, len(resolved))
	for _, e := range resolved {
		key := entryDimKey{
			holder:           e.AccountHolder,
			currencyID:       e.currencyID,
			classificationID: e.classificationID,
			entryType:        flipEntryType(e.EntryType),
		}
		if _, seen := newByDim[key]; !seen {
			order = append(order, key)
		}
		newByDim[key] = newByDim[key].Add(e.Amount)
	}

	for _, key := range order {
		originalAmount, ok := originalByDim[key]
		if !ok {
			return fmt.Errorf(
				"postgres: post journal: reversal_of %q: entry (holder %d, currency %d, classification %d, %s) does not reverse any entry of the referenced journal: %w",
				originalUID, key.holder, key.currencyID, key.classificationID, flipEntryType(key.entryType), core.ErrInvalidInput,
			)
		}
		if alreadyReversed[key].Add(newByDim[key]).GreaterThan(originalAmount) {
			return fmt.Errorf(
				"postgres: post journal: reversal_of %q: dimension (holder %d, currency %d, classification %d, %s): cumulative reversed %s + this journal's %s would exceed original amount %s: %w",
				originalUID, key.holder, key.currencyID, key.classificationID, key.entryType,
				alreadyReversed[key], newByDim[key], originalAmount, core.ErrConflict,
			)
		}
	}
	return nil
}
