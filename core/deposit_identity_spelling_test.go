package core_test

import (
	"maps"
	"testing"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// TestNormalizeDepositIdentityMetadata pins I-71's spelling half:
// uq_bookings_deposit_identity and the sibling lookup compare metadata text
// verbatim, while the chain-side checks read case / 0X and leading-zero /
// '+' variants as the same log. Same value in another spelling is rewritten
// to the canonical one; a value that is not a valid identity is refused.
func TestNormalizeDepositIdentityMetadata(t *testing.T) {
	const h = "0xabababababababababababababababababababababababababababababababab"
	identity := func(chain, tx, seq string) map[string]string {
		return map[string]string{"chain_id": chain, "tx_hash": tx, "txlog_seq": seq, "block_number": "100"}
	}

	t.Run("canonical and non-identity pass through unchanged", func(t *testing.T) {
		for name, md := range map[string]map[string]string{
			"canonical":          identity("1", h, "1"),
			"canonical zero seq": identity("56", h, "0"),
			// Not an identity row: unconstrained, like the index.
			"no identity triple": {"tx_hash": "0xABC"},
			"nil":                nil,
		} {
			got, err := core.NormalizeDepositIdentityMetadata(md)
			require.NoError(t, err, name)
			assert.Equal(t, md, got, name)
		}
	})

	t.Run("same value in another spelling is normalized", func(t *testing.T) {
		for name, md := range map[string]map[string]string{
			"upper-case hash":    identity("1", "0xABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB", "1"),
			"mixed-case hash":    identity("1", "0xAb"+h[4:], "1"),
			"0X prefix":          identity("1", "0X"+h[2:], "1"),
			"leading zero seq":   identity("1", h, "01"),
			"plus seq":           identity("1", h, "+1"),
			"leading zero chain": identity("01", h, "1"),
			"plus chain":         identity("+1", h, "1"),
		} {
			before := maps.Clone(md)
			got, err := core.NormalizeDepositIdentityMetadata(md)
			require.NoError(t, err, name)
			assert.Equal(t, identity("1", h, "1"), got, name)
			assert.Equal(t, before, md, "%s: the input map is not mutated", name)
		}
		got, err := core.NormalizeDepositIdentityMetadata(identity("1", h, "-0"))
		require.NoError(t, err)
		assert.Equal(t, "0", got["txlog_seq"], "-0 parses as 0")
		got, err = core.NormalizeDepositIdentityMetadata(identity("1", "0XReorgTx", "1"))
		require.NoError(t, err)
		assert.Equal(t, "0xreorgtx", got["tx_hash"], "digits are lowercased, not hex-checked")
	})

	t.Run("invalid values are refused", func(t *testing.T) {
		for name, md := range map[string]map[string]string{
			"no prefix":         identity("1", h[2:], "1"),
			"non-numeric chain": identity("ethereum", h, "1"),
			"non-numeric seq":   identity("1", h, "one"),
			"space seq":         identity("1", h, " 1"),
		} {
			_, err := core.NormalizeDepositIdentityMetadata(md)
			assert.ErrorIs(t, err, core.ErrInvalidInput, name)
		}
	})

	// The create path's Validate refuses the same invalid values and lets
	// normalizable spellings through.
	in := core.CreateBookingInput{
		ClassificationCode: "deposit", AccountHolder: 1, CurrencyUID: "c", Amount: decimal.NewFromInt(1),
		IdempotencyKey: "k", Metadata: identity("1", h[2:], "1"),
	}
	assert.ErrorIs(t, in.Validate(), core.ErrInvalidInput)
	in.Metadata = identity("01", "0X"+h[2:], "+1")
	assert.NoError(t, in.Validate())
}
