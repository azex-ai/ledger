// Package core: onchain.go
//
// Types for the crypto deposit + sweep bundle
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md). core only sees
// ports and value types here -- RPC polling, transaction signing, and event
// log parsing live in the chains/evm adapter module; service/ orchestrates
// between them and the existing Booker/JournalWriter ports.
package core

import (
	"fmt"
	"time"

	"github.com/shopspring/decimal"
)

// DepositAddress is the CREATE2-derived custody address registered to one
// account holder (design doc §2). AccountHolder is 1:1 with Address -- salt
// = bytes32(holder), so a holder can never be issued a second address, and
// re-deriving with the same (Factory, InitHash) always yields the same
// Address, on every EVM chain that factory is deployed to.
type DepositAddress struct {
	UID           string `json:"uid"`
	AccountHolder int64  `json:"account_holder"`
	// Address is EIP-55 checksum-cased, as produced by DeriveDepositAddress.
	Address string `json:"address"`
	// Factory and InitHash are the derivation fingerprint recorded at
	// registration time, for audit -- if a consumer ever redeploys the
	// factory or changes the proxy init code, existing rows keep the
	// fingerprint they were actually derived under.
	Factory   string    `json:"factory"`
	InitHash  string    `json:"init_hash"`
	CreatedAt time.Time `json:"created_at"`
}

// AddressRegistrationInput is the input to AddressRegistry.EnsureAddress.
// The caller (service/) derives Address via DeriveDepositAddress before
// calling -- the registry store never derives addresses itself, it only
// persists and looks them up.
type AddressRegistrationInput struct {
	AccountHolder int64  `json:"account_holder"`
	Address       string `json:"address"`
	Factory       string `json:"factory"`
	InitHash      string `json:"init_hash"`
}

func (i AddressRegistrationInput) Validate() error {
	if i.AccountHolder <= 0 {
		return fmt.Errorf("core: deposit address: account_holder must be positive: %w", ErrInvalidInput)
	}
	if i.Address == "" {
		return fmt.Errorf("core: deposit address: address required: %w", ErrInvalidInput)
	}
	if i.Factory == "" {
		return fmt.Errorf("core: deposit address: factory required: %w", ErrInvalidInput)
	}
	if i.InitHash == "" {
		return fmt.Errorf("core: deposit address: init_hash required: %w", ErrInvalidInput)
	}
	return nil
}

// ChainCursor is the watcher's log-scan progress for one chain (design doc
// §3/§6): a restart resumes from LastScannedBlock instead of rescanning from
// genesis or silently skipping unseen blocks. A stalled cursor (not
// advancing) is the `chain_cursor_lag` signal callers should alert on.
type ChainCursor struct {
	ChainID          int64     `json:"chain_id"`
	LastScannedBlock int64     `json:"last_scanned_block"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// DepositSighting is the normalized shape both ingestion paths -- the
// chains/evm watcher polling eth_getLogs, and the channel/onchain webhook --
// produce before handing off to the caller's IngestDeposit orchestration
// (design doc §3). It carries only stable identity fields; anything that
// changes across observations of the same transfer (confirmations, the
// block height it was seen at) belongs in transition metadata, not here --
// otherwise the two ingestion paths would derive different idempotency
// keys for what is otherwise the same sighting.
type DepositSighting struct {
	ChainID int64  `json:"chain_id"`
	TxHash  string `json:"tx_hash"`
	// TxLogSeq is this Transfer log's ordinal position among the logs in
	// TxHash that credit one of our registered addresses -- deliberately NOT
	// the chain's block-level log_index, which is reassigned across a reorg
	// and would silently mint a fresh idempotency key for an
	// already-recorded transfer (design doc §3, booking idempotency key =
	// deposit-{chain_id}-{tx_hash}-{txlog_seq}).
	TxLogSeq int32 `json:"txlog_seq"`
	// Token is the ERC-20 contract address that emitted the Transfer log.
	Token  string          `json:"token"`
	From   string          `json:"from"`
	To     string          `json:"to"`
	Amount decimal.Decimal `json:"amount"`
	// Confirmations is the sighting's block-confirmation count at the time it
	// was observed; the caller compares it against the chain's configured
	// threshold to decide whether to advance the booking to confirmed.
	Confirmations int32 `json:"confirmations"`
	// BlockNumber is the block the transfer log was mined in. Unlike
	// Confirmations (which is only valid at the moment of observation), this
	// is a stable value the ingestion orchestration persists on the deposit
	// booking so a later recheck can recompute confirmations as
	// latest-block minus BlockNumber, without needing to re-scan the
	// original log range (service/onchain.go's pending/confirming recheck
	// loop, design doc §3 "pending booking 的确认数推进").
	BlockNumber int64 `json:"block_number"`
}

func (s DepositSighting) Validate() error {
	if s.ChainID <= 0 {
		return fmt.Errorf("core: deposit sighting: chain_id must be positive: %w", ErrInvalidInput)
	}
	if s.TxHash == "" {
		return fmt.Errorf("core: deposit sighting: tx_hash required: %w", ErrInvalidInput)
	}
	if s.TxLogSeq < 0 {
		return fmt.Errorf("core: deposit sighting: txlog_seq must not be negative: %w", ErrInvalidInput)
	}
	if s.Token == "" {
		return fmt.Errorf("core: deposit sighting: token required: %w", ErrInvalidInput)
	}
	if s.To == "" {
		return fmt.Errorf("core: deposit sighting: to required: %w", ErrInvalidInput)
	}
	if !s.Amount.IsPositive() {
		return fmt.Errorf("core: deposit sighting: amount must be positive: %w", ErrInvalidInput)
	}
	if s.Confirmations < 0 {
		return fmt.Errorf("core: deposit sighting: confirmations must not be negative: %w", ErrInvalidInput)
	}
	return nil
}

// IngestDeadLetter is a deposit sighting that IngestDeposit could not
// idempotently reconcile (CreateBooking returned ErrConflict -- design doc
// §6, a normalization bug signal, not a transient error). Read-only ops
// model for on-call triage; written by postgres.IngestDeadLetterStore.
type IngestDeadLetter struct {
	UID            string    `json:"uid"`
	ChainID        int64     `json:"chain_id"`
	TxHash         string    `json:"tx_hash"`
	TxLogSeq       int32     `json:"txlog_seq"`
	IdempotencyKey string    `json:"idempotency_key"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
}

// SweepNativeToken is the sentinel token key for a chain's native asset
// (ETH, ...) in ChainConfig.SweepTokens / SweepPolicy.Token -- native assets
// have no ERC-20 contract address to key by.
const SweepNativeToken = "native"

// SweepTarget is one registered deposit address the sweep job is collecting
// from, passed to Sweeper.BatchSweep. AccountHolder rides along with Address
// because the factory's on-chain batchSweep ABI takes CREATE2 salts (account
// holder ids), not addresses -- CREATE2 derivation is one-way, so the
// chains/evm adapter cannot recover a holder's salt from its address alone;
// the caller (service/onchain.go, which already has both from the address
// registry) must supply it.
type SweepTarget struct {
	Address       string `json:"address"`
	AccountHolder int64  `json:"account_holder"`
}

// TokenConfig maps one ERC-20 contract address on a chain to the ledger
// currency it credits (deposit side) or is swept as (sweep side), plus the
// token's on-chain decimals so adapter code can normalize raw integer
// amounts into decimal.Decimal.
type TokenConfig struct {
	// TokenAddress is lowercase-normalized by convention; SweepNativeToken
	// for the chain's native asset.
	TokenAddress string `json:"token_address"`
	CurrencyCode string `json:"currency_code"`
	// Decimals is the token contract's on-chain decimals() value (e.g. 6 for
	// USDT/USDC, 18 for most ERC-20s and native ETH).
	Decimals int32 `json:"decimals"`
}

// ChainConfig is one chain's onchain deposit + sweep parameters, injected by
// the consumer's composition root -- core and service never read this from
// the environment directly (abstractions.md Environment Parity).
type ChainConfig struct {
	ChainID int64 `json:"chain_id"`
	// Confirmations is the number of block confirmations required before a
	// pending deposit booking transitions to confirmed.
	Confirmations int32 `json:"confirmations"`
	// Factory / InitHash are this chain's deployed DepositFactory address and
	// DepositProxy init code hash -- the CREATE2 derivation fingerprint (see
	// DeriveDepositAddress). The same pair must be deployed at the same
	// addresses on every chain in the set for a holder's address to be
	// identical across all of them.
	Factory  string `json:"factory"`
	InitHash string `json:"init_hash"`
	// CreditTokens is the deposit-side allowlist (design doc §0: USDT/USDC
	// only this period). Keyed by lowercase token contract address; a
	// Transfer log whose token is not in this map is ignored (logged, not
	// booked).
	CreditTokens map[string]TokenConfig `json:"credit_tokens"`
	// SweepTokens is the collection-side allowlist, independent of
	// CreditTokens -- it may include native and tokens that are never
	// credited to any holder but are still worth sweeping to treasury.
	// Keyed by lowercase token contract address, or SweepNativeToken.
	SweepTokens map[string]TokenConfig `json:"sweep_tokens"`
}

// ChainSet is the full multi-chain configuration a consumer injects into the
// onchain service, keyed by chain ID.
type ChainSet map[int64]ChainConfig

// SweepPolicy governs when the sweep job collects a chain+token's registered
// addresses into the factory's treasury (design doc §4). One policy per
// (ChainID, Token) pair.
type SweepPolicy struct {
	ChainID int64  `json:"chain_id"`
	Token   string `json:"token"` // contract address, or SweepNativeToken
	// MinThreshold is the minimum balance a single address must hold before
	// it is worth including in a sweep batch. Must be set well above the
	// batch's per-address gas cost, or dust deposits become a standing gas
	// drain -- and, since addresses are predictable (salt=holder, factory
	// public), a griefing vector (design doc §5-2).
	MinThreshold decimal.Decimal `json:"min_threshold"`
	// GasCeiling is the max gas price (wei) the sweep job will pay; a batch
	// is skipped (not failed) for this interval if the current price exceeds
	// it.
	GasCeiling decimal.Decimal `json:"gas_ceiling"`
	// BatchLimit bounds how many addresses one sweep transaction collects
	// from.
	BatchLimit int32 `json:"batch_limit"`
	// Interval is how often the sweep job re-evaluates this (ChainID, Token).
	Interval time.Duration `json:"interval"`
}

func (p SweepPolicy) Validate() error {
	if p.ChainID <= 0 {
		return fmt.Errorf("core: sweep policy: chain_id must be positive: %w", ErrInvalidInput)
	}
	if p.Token == "" {
		return fmt.Errorf("core: sweep policy: token required: %w", ErrInvalidInput)
	}
	if p.MinThreshold.IsNegative() {
		return fmt.Errorf("core: sweep policy: min_threshold must not be negative: %w", ErrInvalidInput)
	}
	if p.GasCeiling.IsNegative() {
		return fmt.Errorf("core: sweep policy: gas_ceiling must not be negative: %w", ErrInvalidInput)
	}
	if p.BatchLimit <= 0 {
		return fmt.Errorf("core: sweep policy: batch_limit must be positive: %w", ErrInvalidInput)
	}
	if p.Interval <= 0 {
		return fmt.Errorf("core: sweep policy: interval must be positive: %w", ErrInvalidInput)
	}
	return nil
}

// ReorgPolicy governs how the watcher reacts when a previously confirmed
// deposit's transaction disappears from the canonical chain (design doc §6).
type ReorgPolicy string

const (
	// ReorgPolicyManual (the default) only alerts on-call via a
	// deposit.reorged event; a human decides whether/how to reverse the
	// booking's journal.
	ReorgPolicyManual ReorgPolicy = "manual"
	// ReorgPolicyAutoReverse automatically posts a reversal journal and
	// transitions the booking to reversed. A false positive (RPC blip,
	// lagging node) auto-debits the user -- selecting this policy is an
	// explicit risk acceptance by the consumer, not a safer default.
	ReorgPolicyAutoReverse ReorgPolicy = "auto_reverse"
)

func (p ReorgPolicy) IsValid() bool {
	return p == ReorgPolicyManual || p == ReorgPolicyAutoReverse
}
