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

func TestReorgPolicy_IsValid(t *testing.T) {
	assert.True(t, ReorgPolicyManual.IsValid())
	assert.True(t, ReorgPolicyAutoReverse.IsValid())
	assert.False(t, ReorgPolicy("bogus").IsValid())
	assert.False(t, ReorgPolicy("").IsValid())
}
