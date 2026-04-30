package core

import (
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Property: any balanced N-entry journal with random amounts always validates,
// and Totals() reports the same per-side sum that the entries describe.
//
// This is the open-source ledger's headline invariant: 借贷恒等. It must hold
// regardless of entry ordering, count, or which side the random amount lands.
func TestJournalInvariant_BalancedRandomEntries(t *testing.T) {
	rng := rand.New(rand.NewSource(0xC0FFEE))

	for trial := range 200 {
		n := 1 + rng.Intn(20) // 1..20 debit entries (mirrored on credit side)
		entries := make([]EntryInput, 0, n*2)
		want := decimal.Zero

		for i := range n {
			// 18 fractional digits (NUMERIC(30,18) max).
			amt := decimal.New(int64(rng.Intn(1_000_000_000)+1), -int32(rng.Intn(19)))
			want = want.Add(amt)
			entries = append(entries,
				EntryInput{AccountHolder: int64(i + 1), CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: amt},
				EntryInput{AccountHolder: -int64(i + 1), CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: amt},
			)
		}

		// Shuffle so the test doesn't depend on debit-then-credit order.
		rng.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })

		input := JournalInput{
			JournalTypeID:  1,
			IdempotencyKey: fmt.Sprintf("trial-%d", trial),
			Entries:        entries,
		}
		require.NoError(t, input.Validate(), "trial %d: balanced journal must validate", trial)

		debit, credit := input.Totals()
		assert.True(t, debit.Equal(want), "trial %d: debit total mismatch", trial)
		assert.True(t, credit.Equal(want), "trial %d: credit total mismatch", trial)
		assert.True(t, debit.Equal(credit), "trial %d: debit must equal credit", trial)
	}
}

// Property: any unbalanced delta makes Validate return ErrUnbalancedJournal
// (and NOT ErrInvalidInput — these are semantically different; consumers may
// retry on input-shape errors but never on accounting violations).
func TestJournalInvariant_UnbalancedAlwaysRejected(t *testing.T) {
	rng := rand.New(rand.NewSource(1337))

	for trial := range 100 {
		// Amount large enough that the smallest possible drift never makes
		// the credit side non-positive (which would trip the "amount must be
		// positive" guard before the balance check).
		amt := decimal.New(int64(rng.Intn(1_000_000)+1_000_000), 0)
		// drift between 1e-18 and 1.0
		drift := decimal.New(1, -int32(rng.Intn(18)))
		credit := amt.Sub(drift)
		input := JournalInput{
			JournalTypeID:  1,
			IdempotencyKey: fmt.Sprintf("drift-%d", trial),
			Entries: []EntryInput{
				{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: amt},
				{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: credit},
			},
		}

		err := input.Validate()
		require.Error(t, err, "trial %d: must reject", trial)
		assert.True(t, errors.Is(err, ErrUnbalancedJournal), "trial %d: must wrap ErrUnbalancedJournal, got %v", trial, err)
		assert.False(t, errors.Is(err, ErrInvalidInput), "trial %d: unbalanced must not be classed as invalid input", trial)
	}
}

// Multi-currency invariant: each currency must balance independently. A
// journal that's balanced "globally" but skewed inside a single currency
// must be rejected.
func TestJournalInvariant_MultiCurrencyEachMustBalance(t *testing.T) {
	// Currency 1 unbalanced (100 debit / 50 credit), currency 2 reverses
	// the asymmetry (50 debit / 100 credit) so global totals match.
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "multi-currency-skew",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(100)},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: 2, CurrencyID: 2, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(50)},
			{AccountHolder: -2, CurrencyID: 2, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(100)},
		},
	}

	err := input.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnbalancedJournal))
	// Validate iterates currencies in ascending ID order, so currency 1
	// is the first one to fail.
	assert.Contains(t, err.Error(), "currency 1 unbalanced")
}

// 18-digit precision must round-trip without rounding noise. NUMERIC(30,18)
// is the SQL contract; the Go validator must agree.
func TestJournalInvariant_HighPrecisionAmounts(t *testing.T) {
	amt := decimal.RequireFromString("0.000000000000000001") // 1e-18
	input := JournalInput{
		JournalTypeID:  1,
		IdempotencyKey: "tx-precision",
		Entries: []EntryInput{
			{AccountHolder: 1, CurrencyID: 1, ClassificationID: 1, EntryType: EntryTypeDebit, Amount: amt},
			{AccountHolder: -1, CurrencyID: 1, ClassificationID: 2, EntryType: EntryTypeCredit, Amount: amt},
		},
	}
	require.NoError(t, input.Validate())

	debit, credit := input.Totals()
	assert.True(t, debit.Equal(amt))
	assert.True(t, credit.Equal(amt))
}

// Many entries on the same (holder, classification) side must aggregate
// correctly, not deduplicate.
func TestJournalInvariant_RepeatedEntriesAggregate(t *testing.T) {
	const repeat = 50
	entries := make([]EntryInput, 0, repeat+1)
	for range repeat {
		entries = append(entries, EntryInput{
			AccountHolder: 1, CurrencyID: 1, ClassificationID: 1,
			EntryType: EntryTypeDebit, Amount: decimal.NewFromInt(2),
		})
	}
	entries = append(entries, EntryInput{
		AccountHolder: -1, CurrencyID: 1, ClassificationID: 2,
		EntryType: EntryTypeCredit, Amount: decimal.NewFromInt(repeat * 2),
	})

	input := JournalInput{JournalTypeID: 1, IdempotencyKey: "tx-repeated", Entries: entries}
	require.NoError(t, input.Validate())

	debit, credit := input.Totals()
	assert.True(t, debit.Equal(decimal.NewFromInt(repeat*2)))
	assert.True(t, credit.Equal(decimal.NewFromInt(repeat*2)))
}

// FuzzJournalValidate explores arbitrary entry shapes. The contract:
// Validate must always return an error OR nil — never panic. If it returns
// nil, Totals() must report equal debit and credit.
//
// Run with: go test ./core -run=^$ -fuzz=FuzzJournalValidate -fuzztime=10s
func FuzzJournalValidate(f *testing.F) {
	// Seeds: known-good and known-bad shapes.
	f.Add(int64(1), int64(1), int64(1), "tx", "100", "100")
	f.Add(int64(0), int64(1), int64(1), "", "0", "100")
	f.Add(int64(1), int64(1), int64(1), "tx", "100", "50")
	f.Add(int64(1), int64(1), int64(1), "tx", "1.000000000000000001", "1.000000000000000001")

	f.Fuzz(func(t *testing.T, holder, currencyID, classificationID int64, idemKey, debitAmt, creditAmt string) {
		dr, drErr := decimal.NewFromString(debitAmt)
		cr, crErr := decimal.NewFromString(creditAmt)
		if drErr != nil || crErr != nil {
			t.Skip("non-decimal seed")
		}

		input := JournalInput{
			JournalTypeID:  1,
			IdempotencyKey: idemKey,
			Entries: []EntryInput{
				{AccountHolder: holder, CurrencyID: currencyID, ClassificationID: classificationID, EntryType: EntryTypeDebit, Amount: dr},
				{AccountHolder: -holder, CurrencyID: currencyID, ClassificationID: classificationID, EntryType: EntryTypeCredit, Amount: cr},
			},
		}

		err := input.Validate()
		if err == nil {
			d, c := input.Totals()
			if !d.Equal(c) {
				t.Fatalf("Validate accepted unbalanced journal: debit=%s credit=%s", d, c)
			}
		}
	})
}
