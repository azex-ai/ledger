package core

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddressRegistrationInput_Validate(t *testing.T) {
	valid := AddressRegistrationInput{
		AccountHolder: 1001,
		Address:       "0xB3e7eA5de7C24b4e89b1AC454f02a42DBAE0BFc0",
		Factory:       testFactory,
		InitHash:      testInitHash,
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*AddressRegistrationInput)
	}{
		{"zero holder", func(i *AddressRegistrationInput) { i.AccountHolder = 0 }},
		{"negative holder", func(i *AddressRegistrationInput) { i.AccountHolder = -1 }},
		{"missing address", func(i *AddressRegistrationInput) { i.Address = "" }},
		{"missing factory", func(i *AddressRegistrationInput) { i.Factory = "" }},
		{"missing init_hash", func(i *AddressRegistrationInput) { i.InitHash = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestDepositSighting_Validate(t *testing.T) {
	valid := DepositSighting{
		ChainID:       1,
		TxHash:        "0xabc",
		TxLogSeq:      0,
		Token:         "0xusdt",
		From:          "0xfrom",
		To:            "0xto",
		Amount:        decimal.NewFromInt(100),
		Confirmations: 3,
		BlockNumber:   1000,
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*DepositSighting)
	}{
		{"zero chain_id", func(s *DepositSighting) { s.ChainID = 0 }},
		{"missing tx_hash", func(s *DepositSighting) { s.TxHash = "" }},
		{"negative txlog_seq", func(s *DepositSighting) { s.TxLogSeq = -1 }},
		{"missing token", func(s *DepositSighting) { s.Token = "" }},
		{"missing to", func(s *DepositSighting) { s.To = "" }},
		{"non-positive amount", func(s *DepositSighting) { s.Amount = decimal.Zero }},
		{"negative confirmations", func(s *DepositSighting) { s.Confirmations = -1 }},
		// C1 regression: a zero (or negative) BlockNumber must fail
		// validation -- this is the exact value both ingestion producers
		// (chains/evm's watcher, channel/onchain's webhook) used to leave
		// unset, which silently made recheckOneDeposit compute confirmations
		// as latest-0+1 and bypass the confirmation threshold entirely.
		{"zero block_number", func(s *DepositSighting) { s.BlockNumber = 0 }},
		{"negative block_number", func(s *DepositSighting) { s.BlockNumber = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

func TestSweepPolicy_Validate(t *testing.T) {
	valid := SweepPolicy{
		ChainID:      1,
		Token:        SweepNativeToken,
		MinThreshold: decimal.NewFromInt(10),
		GasCeiling:   decimal.NewFromInt(20),
		BatchLimit:   50,
		Interval:     time.Minute,
	}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*SweepPolicy)
	}{
		{"zero chain_id", func(p *SweepPolicy) { p.ChainID = 0 }},
		{"missing token", func(p *SweepPolicy) { p.Token = "" }},
		{"negative min_threshold", func(p *SweepPolicy) { p.MinThreshold = decimal.NewFromInt(-1) }},
		{"negative gas_ceiling", func(p *SweepPolicy) { p.GasCeiling = decimal.NewFromInt(-1) }},
		{"zero batch_limit", func(p *SweepPolicy) { p.BatchLimit = 0 }},
		{"zero interval", func(p *SweepPolicy) { p.Interval = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			tc.mutate(&input)
			err := input.Validate()
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

// TestTokenConfig_AutoCreditCeilingConfigured pins the M3.1 secure-by-default
// sentinel contract (design doc §9.2 addendum, MJ1): zero is "unconfigured"
// (service.Onchain.Run refuses to start on it), a positive ceiling and the
// explicit UnboundedAutoCredit sentinel are both "deliberately configured".
func TestTokenConfig_AutoCreditCeilingConfigured(t *testing.T) {
	cases := []struct {
		name     string
		ceiling  decimal.Decimal
		expected bool
	}{
		{"zero value (never set)", decimal.Zero, false},
		{"positive ceiling", decimal.NewFromInt(10_000), true},
		{"explicit unbounded sentinel", UnboundedAutoCredit, true},
		{"arbitrary negative is not the sentinel", decimal.NewFromInt(-2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := TokenConfig{AutoCreditCeiling: tc.ceiling}
			assert.Equal(t, tc.expected, cfg.AutoCreditCeilingConfigured())
		})
	}
}

func TestReorgPolicy_IsValid(t *testing.T) {
	assert.True(t, ReorgPolicyManual.IsValid())
	assert.True(t, ReorgPolicyAutoReverse.IsValid())
	assert.False(t, ReorgPolicy("bogus").IsValid())
	assert.False(t, ReorgPolicy("").IsValid())
}

// TestSweepPolicy_Validate_RejectsWeiShapedGasCeiling pins the machine half
// of G-M3 (onchain-money-path.md Major): GasCeiling's own field doc said wei
// while the quantity it is compared against (Sweeper.GasPrice) reports gwei,
// so a consumer following the doc configured a ceiling 10^9 too high and the
// only gate bounding sweep spend silently stopped firing. A documentation
// contradiction becomes a startup rejection (working-agreements §5).
func TestSweepPolicy_Validate_RejectsWeiShapedGasCeiling(t *testing.T) {
	weiShaped := SweepPolicy{
		ChainID:      1,
		Token:        SweepNativeToken,
		MinThreshold: decimal.NewFromInt(1),
		GasCeiling:   decimal.RequireFromString("50000000000"), // 50 gwei written in wei
		BatchLimit:   10,
		Interval:     time.Minute,
	}
	err := weiShaped.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "wei value in a gwei field")

	// A generous but plausible gwei ceiling still passes.
	gweiShaped := weiShaped
	gweiShaped.GasCeiling = decimal.NewFromInt(500)
	require.NoError(t, gweiShaped.Validate())
}

// TestTokenConfig_Validate pins G-M7's configured-value half: Decimals is
// the sole input to the adapter's raw-amount normalization
// (NewFromBigInt(raw, -Decimals)), so a negative value multiplies every
// credited amount by 10^n rather than dividing.
func TestTokenConfig_Validate(t *testing.T) {
	require.NoError(t, TokenConfig{Decimals: 0}.Validate())
	require.NoError(t, TokenConfig{Decimals: 18}.Validate())

	err := TokenConfig{Decimals: -1}.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "multiplies")

	err = TokenConfig{Decimals: 37}.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
}

// TestDepositReorg_IsOpen pins the no-NULL encoding of an unresolved anomaly
// (core.DepositReorg): the epoch default means "open", and call sites must
// not have to know that.
func TestDepositReorg_IsOpen(t *testing.T) {
	assert.True(t, DepositReorg{}.IsOpen(), "the zero value is an open anomaly")
	assert.True(t, DepositReorg{ResolvedAt: time.Unix(0, 0)}.IsOpen(), "epoch means open")
	assert.False(t, DepositReorg{ResolvedAt: time.Now()}.IsOpen())
}
