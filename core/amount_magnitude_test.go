package core_test

// The pins for ValidateAmountMagnitude and for every entry point it
// guards.
//
// Found by FuzzJournalValidate inside the 30s budget CI already runs
// (2026-09-03 W5, verifying whether that budget was effective -- it was).
// `{"amount": "1E999999999"}` is nineteen bytes. shopspring/decimal stores
// it lazily as coefficient 1 with exponent 999999999, so every cheap
// operation on it -- parse, IsPositive, Equal, Add, Truncate -- is
// microseconds, and both JournalInput.Validate and the per-currency
// precision check passed it. Nothing looked at the magnitude until pgx
// rendered the decimal to bind a NUMERIC parameter, at which point String()
// expands a billion digits: PostJournal did not return after ninety
// seconds, spending unbounded CPU in math/big and allocating without bound.
//
// Every assertion below carries a TIME bound, because "returns an error"
// is not the property at stake -- "returns quickly" is. A check that
// eventually rejected the amount after expanding it would satisfy a plain
// require.Error and still be the denial of service.

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// pathologicalAmounts are amounts that are cheap to hold and ruinous to
// render. The second is the exact input FuzzJournalValidate produced.
var pathologicalAmounts = map[string]string{
	"1E999999999":  "the minimal reproduction: nineteen bytes, a billion integer digits",
	"10E777777070": "the input FuzzJournalValidate found (core/testdata/fuzz/FuzzJournalValidate/7ec58597750a4f04)",
	"1E13":         "just past the NUMERIC(30,18) integer width -- the boundary, not the extreme",
	"-1E999999999": "sign must not be a way around it",
}

// rejectionBudget is how long a magnitude check may take. The real check is
// three field reads and a small integer's digit count; 100ms is four orders
// of magnitude of headroom, and the failure it guards against does not
// terminate at all, so there is no risk of a flaky middle ground.
const rejectionBudget = 100 * time.Millisecond

// mustReject runs validate and requires it to fail WITHIN the budget.
func mustReject(t *testing.T, entry, amount, why string, validate func(decimal.Decimal) error) {
	t.Helper()
	d, err := decimal.NewFromString(amount)
	require.NoErrorf(t, err, "%s: the wire parses %q cheaply -- that is the whole problem", entry, amount)

	done := make(chan error, 1)
	go func() { done <- validate(d) }()

	select {
	case verr := <-done:
		require.Errorf(t, verr, "%s accepted amount %q (%s). NUMERIC(30,18) cannot store it, and something downstream "+
			"has to expand it to find that out", entry, amount, why)
		assert.ErrorIsf(t, verr, core.ErrInvalidInput,
			"%s rejected %q with an error that is not ErrInvalidInput, so the HTTP boundary cannot map it: %v", entry, amount, verr)
	case <-time.After(rejectionBudget):
		t.Fatalf("%s did not reject amount %q (%s) within %v.\n\n"+
			"Returning an error eventually is not the property: the value is cheap to hold and ruinous to render, so a "+
			"check that expands it before refusing it IS the denial of service. The check must read the exponent and "+
			"the coefficient's digit count and nothing else", entry, amount, why, rejectionBudget)
	}
}

func TestValidateAmountMagnitude_RejectsWhatNumericCannotStore(t *testing.T) {
	for amount, why := range pathologicalAmounts {
		mustReject(t, "ValidateAmountMagnitude", amount, why, func(d decimal.Decimal) error {
			return core.ValidateAmountMagnitude("test", "amount", d)
		})
	}
}

// TestValidateAmountMagnitude_AcceptsWhatNumericCanStore is the control.
// Without it, a check that refused everything would pass every assertion
// above while making the ledger unusable.
func TestValidateAmountMagnitude_AcceptsWhatNumericCanStore(t *testing.T) {
	for _, amount := range []string{
		"0", "1", "-1", "0.000000000000000001", // 18 fractional digits: the exact floor
		"999999999999", "-999999999999", // 12 integer digits: the exact ceiling
		"123456789012.123456789012345678", // both widths at once
		"1E11",                            // scientific notation is fine when the magnitude is (1E12 would be a 13th integer digit)
	} {
		d, err := decimal.NewFromString(amount)
		require.NoError(t, err)
		require.NoErrorf(t, core.ValidateAmountMagnitude("test", "amount", d),
			"%q fits NUMERIC(30,18) and must be accepted -- a bound that refuses storable amounts is a bug, not a guard", amount)
	}
}

// TestEveryAmountEntryPointRejectsAPathologicalAmount is the sibling scan
// made executable.
//
// Fixing this at PostJournal alone would have left the same input reachable
// through Reserve, through a booking, through a pending credit, and -- the
// one that is remotely reachable without a scientific-notation guard in
// front of it -- through the inbound webhook adapter's DepositSighting
// (channel/onchain/evm.go calls decimal.NewFromString on the raw body
// directly; server/amount.go's parseWireAmount, which refuses `e`/`E`, is
// not in that path).
func TestEveryAmountEntryPointRejectsAPathologicalAmount(t *testing.T) {
	entries := map[string]func(decimal.Decimal) error{
		"core.JournalInput.Validate": func(d decimal.Decimal) error {
			in := core.JournalInput{
				JournalTypeUID: "jt", IdempotencyKey: "k",
				Entries: []core.EntryInput{
					{AccountHolder: 1, CurrencyUID: "c", ClassificationUID: "cl", EntryType: core.EntryTypeDebit, Amount: d},
					{AccountHolder: -1, CurrencyUID: "c", ClassificationUID: "cl", EntryType: core.EntryTypeCredit, Amount: d},
				},
			}
			return in.Validate()
		},
		"core.ReserveInput.Validate": func(d decimal.Decimal) error {
			return core.ReserveInput{AccountHolder: 1, CurrencyUID: "c", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.SettleInput.Validate": func(d decimal.Decimal) error {
			return core.SettleInput{ReservationUID: "r", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.SettlePartialInput.Validate": func(d decimal.Decimal) error {
			return core.SettlePartialInput{ReservationUID: "r", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.AddPendingInput.Validate": func(d decimal.Decimal) error {
			return core.AddPendingInput{AccountHolder: 1, CurrencyUID: "c", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.ConfirmPendingInput.Validate": func(d decimal.Decimal) error {
			return core.ConfirmPendingInput{AccountHolder: 1, CurrencyUID: "c", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.CancelPendingInput.Validate": func(d decimal.Decimal) error {
			return core.CancelPendingInput{AccountHolder: 1, CurrencyUID: "c", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.CreateBookingInput.Validate": func(d decimal.Decimal) error {
			return core.CreateBookingInput{
				ClassificationCode: "cl", AccountHolder: 1, CurrencyUID: "c", Amount: d, IdempotencyKey: "k",
			}.Validate()
		},
		"core.TransitionInput.Validate": func(d decimal.Decimal) error {
			return core.TransitionInput{BookingUID: "b", ToStatus: "confirmed", Amount: d, IdempotencyKey: "k"}.Validate()
		},
		"core.AccountPolicyInput.Validate": func(d decimal.Decimal) error {
			return core.AccountPolicyInput{AccountHolder: 1, MinBalance: d}.Validate()
		},
		"core.DepositSighting.Validate (the webhook path)": func(d decimal.Decimal) error {
			return core.DepositSighting{
				ChainID: 1, TxHash: "0xabc", Token: "0xtok", To: "0xaddr", Amount: d, BlockNumber: 1,
			}.Validate()
		},
		"core.SweepPolicy.Validate": func(d decimal.Decimal) error {
			return core.SweepPolicy{ChainID: 1, Token: "0xtok", MinThreshold: d, GasCeiling: d}.Validate()
		},
		"core.TokenConfig.Validate": func(d decimal.Decimal) error {
			return core.TokenConfig{AutoCreditCeiling: d, ReconcileCeiling: d}.Validate()
		},
	}

	// EntryTemplate.Render is the ExecuteTemplate path: its TemplateParams
	// amounts become EntryInputs and it calls JournalInput.Validate itself,
	// so it inherits the gate. Asserted rather than assumed.
	entries["core.EntryTemplate.Render (the ExecuteTemplate path)"] = func(d decimal.Decimal) error {
		tpl := &core.EntryTemplate{
			Code: "t", JournalTypeUID: "jt", IsActive: true,
			Lines: []core.EntryTemplateLine{
				{ClassificationUID: "cl", HolderRole: core.HolderRoleUser, EntryType: core.EntryTypeDebit, AmountKey: "amount"},
				{ClassificationUID: "cl", HolderRole: core.HolderRoleSystem, EntryType: core.EntryTypeCredit, AmountKey: "amount"},
			},
		}
		_, err := tpl.Render(core.TemplateParams{
			HolderID: 1, CurrencyUID: "c", IdempotencyKey: "k",
			Amounts: map[string]decimal.Decimal{"amount": d},
		})
		return err
	}

	require.GreaterOrEqual(t, len(entries), 14,
		"the sibling scan found fourteen caller-supplied amount entry points in core; a table that has drifted below "+
			"that is a table that stopped covering one")

	for name, validate := range entries {
		t.Run(name, func(t *testing.T) {
			for amount, why := range pathologicalAmounts {
				mustReject(t, name, amount, why, validate)
			}
		})
	}
}
