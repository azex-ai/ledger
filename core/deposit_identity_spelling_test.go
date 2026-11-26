package core_test

import (
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"

	"github.com/azex-ai/ledger/core"
)

// TestValidateDepositIdentityMetadata_RejectsNonCanonicalSpelling pins I-71's
// spelling half: uq_bookings_deposit_identity and the sibling lookup compare
// metadata text verbatim, while the chain-side checks read case / 0X and
// leading-zero / '+' variants as the same log.
func TestValidateDepositIdentityMetadata_RejectsNonCanonicalSpelling(t *testing.T) {
	const h = "0xabababababababababababababababababababababababababababababababab"
	identity := func(chain, tx, seq string) map[string]string {
		return map[string]string{"chain_id": chain, "tx_hash": tx, "txlog_seq": seq, "block_number": "100"}
	}

	assert.NoError(t, core.ValidateDepositIdentityMetadata(identity("1", h, "1")), "canonical")
	assert.NoError(t, core.ValidateDepositIdentityMetadata(identity("56", h, "0")), "canonical zero seq")
	assert.NoError(t, core.ValidateDepositIdentityMetadata(map[string]string{"tx_hash": "0xABC"}),
		"no identity triple: not an identity row, unconstrained like the index")
	assert.NoError(t, core.ValidateDepositIdentityMetadata(nil))

	for name, md := range map[string]map[string]string{
		"upper-case hash":    identity("1", "0xABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB", "1"),
		"mixed-case hash":    identity("1", "0xAb"+h[4:], "1"),
		"0X prefix":          identity("1", "0X"+h[2:], "1"),
		"no prefix":          identity("1", h[2:], "1"),
		"leading zero seq":   identity("1", h, "01"),
		"plus seq":           identity("1", h, "+1"),
		"minus zero seq":     identity("1", h, "-0"),
		"leading zero chain": identity("01", h, "1"),
		"plus chain":         identity("+1", h, "1"),
		"non-numeric chain":  identity("ethereum", h, "1"),
		"space seq":          identity("1", h, " 1"),
	} {
		assert.ErrorIs(t, core.ValidateDepositIdentityMetadata(md), core.ErrInvalidInput, name)
	}

	// The create path runs the same check.
	in := core.CreateBookingInput{
		ClassificationCode: "deposit", AccountHolder: 1, CurrencyUID: "c", Amount: decimal.NewFromInt(1),
		IdempotencyKey: "k", Metadata: identity("1", "0X"+h[2:], "1"),
	}
	assert.ErrorIs(t, in.Validate(), core.ErrInvalidInput)
}
