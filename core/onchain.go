// Package core: onchain.go
//
// Types for the crypto deposit + sweep bundle
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md). core only sees
// ports and value types here -- RPC polling, transaction signing, and event
// log parsing live in the chains/evm adapter module; service/ orchestrates
// between them and the existing Booker/JournalWriter ports.
package core

import (
	"context"
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
	// TxLogSeq is this Transfer log's zero-based position among ALL logs in
	// TxHash's transaction receipt (design doc §3, booking idempotency key =
	// deposit-{chain_id}-{tx_hash}-{txlog_seq}).
	//
	// Two properties make that the only admissible definition, and both were
	// violated by the previous one ("position among the logs in TxHash that
	// credit one of OUR registered addresses" -- G-C2,
	// docs/audits/2026-09-02-deep-audit/onchain-money-path.md):
	//
	//  1. It must not depend on who is looking. The watcher queries every
	//     registered address at once; a registration rescan queries exactly
	//     one. A "position among our hits" therefore differed between the two
	//     paths for any transaction crediting two registered addresses, so
	//     the same transfer derived two different idempotency keys -- booking
	//     it twice, or dead-lettering a legitimate one forever.
	//  2. It must survive a reorg re-mining the same transaction. This is why
	//     it is NOT the chain's block-level log_index, which shifts wholesale
	//     when a transaction lands at a different position in a different
	//     block, minting a fresh key for an already-credited transfer.
	//
	// Receipt position satisfies both: it is a property of the transaction
	// alone. Every producer must use exactly this definition -- the
	// chains/evm watcher derives it from eth_getTransactionReceipt, and an
	// external scanner feeding the channel/onchain webhook must report the
	// same receipt-relative index, not its own scan-order counter.
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
	if err := ValidateAmountMagnitude("deposit sighting", "amount", s.Amount); err != nil {
		return err
	}
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
	// BlockNumber must be a real, positive block height -- both ingestion
	// producers (chains/evm's watcher, channel/onchain's webhook) are
	// required to fill it (see this field's doc comment). A zero value here
	// is not "genesis block", it is "producer forgot to set it", and letting
	// it through silently reintroduces the confirmation-threshold bypass
	// design doc §3 depends on (recheck would compute confirmations against
	// block 0, i.e. always far past any real threshold).
	if s.BlockNumber <= 0 {
		return fmt.Errorf("core: deposit sighting: block_number must be positive: %w", ErrInvalidInput)
	}
	return nil
}

// IngestDeadLetter is a deposit sighting the ingestion path refused to book
// and the forward scan then moved past: a CreateBooking ErrConflict (design
// doc §6, a normalization bug signal, not a transient error), an unregistered
// currency, an amount the currency's exponent cannot represent, or a watcher
// wedged on this sighting for several consecutive ticks. Read-only ops model
// for on-call triage (docs/RUNBOOK.md §18); written by
// postgres.IngestDeadLetterStore.
//
// The row is the ONLY durable trace such a sighting leaves: no booking was
// created, so no recheck loop revisits it, and the cursor is past it, so no
// forward scan sees it again. That is why Sighting is carried here -- a
// replay needs nothing else (service.Onchain.ReplayDeadLetter).
type IngestDeadLetter struct {
	UID            string    `json:"uid"`
	ChainID        int64     `json:"chain_id"`
	TxHash         string    `json:"tx_hash"`
	TxLogSeq       int32     `json:"txlog_seq"`
	IdempotencyKey string    `json:"idempotency_key"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
	// Booked reports whether a booking now exists for this sighting's
	// idempotency key -- i.e. whether the deposit was eventually credited
	// after all, by a replay or because the cause self-healed (a frozen
	// account unfrozen, a closed period reopened). It is what makes the
	// dead-letter table a queue that clears itself rather than an alarm
	// nailed to ON: the row is never rewritten, the answer is recomputed
	// from bookings on every read.
	Booked bool `json:"booked"`
	// Sighting is the deposit sighting as recorded at rejection time, as
	// stored in the row's payload column.
	Sighting DepositSighting `json:"sighting"`
}

// Deposit chain-anomaly kinds recorded on DepositReorg.Kind. Both describe a
// deposit booking whose on-chain reality stopped matching the ledger's
// record, and both need a HUMAN to close them out: neither is something this
// library may resolve on its own (a deep reorg's reversal is
// ReorgPolicyManual's whole point; a recovered shallow-reorg failure cannot
// be un-failed at all, since DepositLifecycle's failed is terminal).
const (
	// ReorgKindDeepReorg is a booking that reached confirmed (its journal is
	// posted, the holder's balance moved) whose transaction is no longer on
	// the canonical chain.
	ReorgKindDeepReorg = "deep_reorg"
	// ReorgKindShallowReorgFailed is a booking the watcher itself failed
	// because its transaction disappeared before reaching the confirmation
	// threshold. That decision is automatic and irreversible (failed is
	// terminal in DepositLifecycle, and the booking's idempotency key
	// resolves every future sighting of the same transfer back to it), so if
	// the transaction turns out to be on chain after all, only a human can
	// make the holder whole -- which they can only do if the decision left a
	// record. G-M1.
	ReorgKindShallowReorgFailed = "shallow_reorg_failed"
)

// DepositReorg is one open (or closed-out) chain anomaly on a deposit
// booking -- the durable half of reorg handling.
//
// Why it is a table and not just an alert (G-M8,
// docs/audits/2026-09-02-deep-audit/onchain-money-path.md): under the default
// ReorgPolicyManual, detection used to be a log line plus a counter
// increment, and the recheck that produced it stopped repeating once the
// booking's block fell outside reorgRecheckWindow -- about 17 minutes on a
// 2s-block chain. By the time an on-call engineer followed RUNBOOK §12's
// "verify against a second source before reversing", the signal telling them
// WHICH booking to look at was gone. A row survives the window, the process,
// and the log retention period; LastSeenAt records that the anomaly is still
// observable, and ResolvedAt is the only way one leaves the queue.
type DepositReorg struct {
	UID        string    `json:"uid"`
	Kind       string    `json:"kind"`
	BookingUID string    `json:"booking_uid"`
	ChainID    int64     `json:"chain_id"`
	TxHash     string    `json:"tx_hash"`
	JournalUID string    `json:"journal_uid"`
	DetectedAt time.Time `json:"detected_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	// ResolvedAt is the Unix epoch while the anomaly is open (the repo's
	// no-NULL convention), and the closing timestamp once an operator
	// resolved it. Use IsOpen rather than comparing timestamps at call sites.
	ResolvedAt time.Time `json:"resolved_at"`
	Resolution string    `json:"resolution"`
}

// IsOpen reports whether this anomaly still needs human attention.
func (r DepositReorg) IsOpen() bool { return r.ResolvedAt.Unix() <= 0 }

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

// UnboundedAutoCredit is the explicit sentinel a consumer sets
// TokenConfig.AutoCreditCeiling to when they deliberately want NO cap on
// this token's auto-credit path -- an explicit, reviewed risk acceptance
// that a single confirmed sighting (the primary RPC's word alone, absent a
// matching second-source reconciliation) may auto-credit any amount. This is
// DISTINCT from the zero value, which means "nobody ever set this field"
// and is refused at startup (service.Onchain.Run) rather than silently
// treated as unbounded -- see AutoCreditCeiling's doc comment and design doc
// §9.2 addendum (M3.1 secure-by-default, docs/bugs/2026-07-11-m3-security-review.md
// MJ1).
var UnboundedAutoCredit = decimal.NewFromInt(-1)

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
	//
	// MUST equal what the token contract's own decimals() returns: this
	// value is the sole input to the adapter's raw-integer -> decimal
	// normalization, so it alone fixes the order of magnitude every deposit
	// of this token is credited at, and getting it wrong in the
	// under-crediting direction produces no signal anywhere (G-M7). Prove it
	// at startup with (*evm.ClientSet).VerifyTokenDecimals -- this library
	// ships no binary, so the composition root owns that call.
	Decimals int32 `json:"decimals"`
	// AutoCreditCeiling is the maximum deposit amount (in ledger currency
	// units) this token may auto-credit through the confirming->confirmed
	// path without pausing for human review (design doc §9.2: M3
	// compensating controls -- the deposit path's RPC oracle is otherwise
	// unbounded trusted). MUST be deliberately set on every
	// ChainConfig.CreditTokens entry -- either to a positive ceiling, or to
	// UnboundedAutoCredit to explicitly accept an unbounded single-source
	// trust model. The zero value ("never touched this field") is NOT
	// "unbounded": service.Onchain.Run refuses to start with any
	// CreditTokens entry left at zero, precisely because that silent
	// default is the pre-M3 unbounded-mint trust model M3 exists to close
	// (see UnboundedAutoCredit's doc comment). Only meaningful on
	// ChainConfig.CreditTokens entries; SweepTokens never route through the
	// review gate (sweep bookings never post a journal, I-19).
	//
	// This gate is decidable only while the token is still IN CreditTokens.
	// A token removed from the allowlist (a delisting, a contract
	// migration, a config rollback) leaves already-confirming bookings
	// behind, and the startup check above can only see tokens that are
	// still configured -- so those bookings' ceilings become unknowable.
	// "Unknowable" is not "unbounded": every in-flight booking of a
	// no-longer-configured token is parked for human review instead
	// (G-M6). Before that, the missing map entry collapsed into a
	// zero-valued TokenConfig, whose non-positive ceilings turned both
	// gates off and auto-credited any amount.
	AutoCreditCeiling decimal.Decimal `json:"auto_credit_ceiling"`
	// ReconcileCeiling is the minimum deposit amount that requires a second,
	// independent-source confirmation (OnchainDeps.DepositConfirmer) before
	// auto-crediting (design doc §9.3). Zero disables the gate even when a
	// DepositConfirmer is configured. Independent of AutoCreditCeiling -- a
	// consumer may set either, both, or neither. Unlike AutoCreditCeiling,
	// leaving this at zero is a legitimate ("no reconciliation gate")
	// choice, not a startup error: the ceiling that actually bounds mint
	// exposure is AutoCreditCeiling.
	ReconcileCeiling decimal.Decimal `json:"reconcile_ceiling"`
	// ReconcileFailureLimit is the number of consecutive
	// DepositConfirmer.ConfirmDeposit errors (not amount mismatches -- those
	// already route straight to review) service.Onchain tolerates before
	// escalating a stuck confirming deposit to review instead of retrying it
	// forever (mi5, docs/bugs/2026-07-11-m3-security-review.md: a long-lived
	// second-source outage must not leave a legitimate deposit's confirming
	// status indistinguishable from "nobody is working on it" --
	// working-agreements §3). MUST be a deliberate positive integer whenever
	// the reconciliation gate is actually active for this token
	// (OnchainDeps.DepositConfirmer configured and ReconcileCeiling
	// positive) -- service.Onchain.Run and ValidateReconcileFailureLimits
	// refuse to start otherwise. Unlike AutoCreditCeiling this is an
	// availability fence, not a mint-safety one, so there is no
	// "explicitly unbounded" sentinel: a consumer that does not want this
	// gate simply leaves ReconcileCeiling at zero or DepositConfirmer nil,
	// which already turns the whole reconciliation mechanism off (design
	// doc §9.3) and makes this field irrelevant.
	ReconcileFailureLimit int32 `json:"reconcile_failure_limit"`
}

// maxTokenDecimals bounds TokenConfig.Decimals. 18 is not headroom, it is
// the ceiling imposed by the other end of the pipe: normalizeAmount produces
// an amount with Decimals decimal places, that amount is booked against a
// ledger currency, and CurrencyInput.Validate caps a currency's Exponent at
// 18. A token configured above 18 therefore has NO currency that can
// represent its non-integer amounts, so every such deposit is refused by
// postgres' precision check and dead-lettered -- a real, confirmed transfer
// silently written off, with the configuration that guaranteed it having
// passed validation (M-2,
// docs/audits/2026-09-03-independent-review/onchain-ops.md).
//
// This used to be 36 ("generous headroom"), which is how the two limits came
// to disagree. The per-token cross-check against the currency's ACTUAL
// exponent -- which may be lower still -- is
// service.Onchain.ValidateTokenPrecision; this constant is what a consumer
// who never calls Run() still gets, since it needs no database.
const maxTokenDecimals = 18

// Validate rejects a TokenConfig whose Decimals cannot describe any real
// token. A negative value is the dangerous one: normalizeAmount computes
// NewFromBigInt(raw, -Decimals), so Decimals=-6 MULTIPLIES the credited
// amount by 10^6 (G-M7). Called by service.Onchain's startup validation and
// by (*evm.ClientSet).VerifyTokenDecimals before it goes to the chain.
func (c TokenConfig) Validate() error {
	if err := ValidateAmountMagnitude("token config", "auto_credit_ceiling", c.AutoCreditCeiling); err != nil {
		return err
	}
	if err := ValidateAmountMagnitude("token config", "reconcile_ceiling", c.ReconcileCeiling); err != nil {
		return err
	}
	if c.Decimals < 0 {
		return fmt.Errorf("core: token config: decimals must not be negative (a negative value multiplies every credited amount by 10^%d): %w", -c.Decimals, ErrInvalidInput)
	}
	if c.Decimals > maxTokenDecimals {
		return fmt.Errorf("core: token config: decimals=%d exceeds the %d maximum -- a ledger currency's exponent caps at %d, so no currency could represent this token's non-integer amounts and every such deposit would be dead-lettered: %w", c.Decimals, maxTokenDecimals, maxTokenDecimals, ErrInvalidInput)
	}
	return nil
}

// AutoCreditCeilingConfigured reports whether AutoCreditCeiling has been
// deliberately set: either to a positive bound, or to the explicit
// UnboundedAutoCredit sentinel. false means "left at the zero value" --
// service.Onchain.Run's startup validation treats that as a configuration
// error for every CreditTokens entry (see AutoCreditCeiling's doc comment).
func (c TokenConfig) AutoCreditCeilingConfigured() bool {
	return c.AutoCreditCeiling.IsPositive() || c.AutoCreditCeiling.Equal(UnboundedAutoCredit)
}

// ChainConfig is one chain's onchain deposit + sweep parameters, injected by
// the consumer's composition root -- core and service never read this from
// the environment directly (abstractions.md Environment Parity).
type ChainConfig struct {
	ChainID int64 `json:"chain_id"`
	// ScanStartBlock is the first block that can contain events from this
	// chain's DepositFactory deployment. Registration rescans start here
	// instead of querying the chain from genesis.
	ScanStartBlock int64 `json:"scan_start_block"`
	// Confirmations is the number of block confirmations required before a
	// pending deposit booking transitions to confirmed.
	//
	// It is ALSO the forward scanner's rollback depth: the watcher never
	// scans past latest-Confirmations+1, so a block whose contents a reorg
	// still might replace is never marked scanned (I-53, G-M2). One
	// consequence for alerting: Metrics.ChainCursorLag's healthy baseline is
	// therefore Confirmations, not 0.
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

// RegistrationRescan is durable progress for the historical scan triggered
// when a deposit address is first registered. One row exists per
// (address, chain), so retries and process restarts resume at NextBlock.
type RegistrationRescan struct {
	UID       string
	ChainID   int64
	Address   string
	NextBlock int64
	Attempts  int32
}

// RegistrationRescanStore persists and leases address-registration rescans.
// Implementations must make Claim safe across multiple service replicas.
//
// AdvanceRegistrationRescan and RetryRegistrationRescan both take
// expectedAttempts: the Attempts value the caller observed on the
// RegistrationRescan it claimed (ClaimRegistrationRescans bumps Attempts by
// one on every claim, including a re-claim after a lease expired). A write
// only applies if the stored row's attempts still equals expectedAttempts --
// the same claim-token-guard shape rollup_queue's MarkRollupProcessed and
// events' UpdateEventDelivered already use (keyed on claimed_until /
// next_attempt_at there; keyed on attempts here since Attempts is the value
// already threaded through RegistrationRescan, so no new column is needed).
// Without it, a worker whose lease outlived its own processing could
// overwrite progress a worker that re-claimed the same row after the lease
// expired already recorded (concurrency.md Major, board #30).
type RegistrationRescanStore interface {
	EnqueueRegistrationRescans(ctx context.Context, jobs []RegistrationRescan) error
	ClaimRegistrationRescans(ctx context.Context, limit int, lease time.Duration) ([]RegistrationRescan, error)
	AdvanceRegistrationRescan(ctx context.Context, uid string, nextBlock int64, completed bool, expectedAttempts int32) error
	RetryRegistrationRescan(ctx context.Context, uid, lastError string, retryAt time.Time, expectedAttempts int32) error
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
	// GasCeiling is the max gas price (GWEI) the sweep job will pay; a batch
	// is skipped (not failed) for this interval if the current price exceeds
	// it.
	//
	// The unit is gwei because that is what the quantity it is compared
	// against -- Sweeper.GasPrice, see its doc comment in core/interfaces.go
	// -- reports. These two MUST stay in the same unit: changing one without
	// the other silently scales this ceiling by 10^9 and the only gate
	// bounding what a sweep may spend stops firing (G-M3; this comment said
	// "wei" until then, so a consumer following it configured a ceiling a
	// billion times too high). Validate below rejects values that look like
	// they were configured in wei.
	GasCeiling decimal.Decimal `json:"gas_ceiling"`
	// BatchLimit bounds how many addresses one sweep transaction collects
	// from.
	BatchLimit int32 `json:"batch_limit"`
	// Interval is how often the sweep job re-evaluates this (ChainID, Token).
	Interval time.Duration `json:"interval"`
}

// maxPlausibleGasCeilingGwei is the upper bound SweepPolicy.Validate accepts
// for GasCeiling. 1e6 gwei = 1000 ETH per 10^9 gas units: orders of magnitude
// above any real gas spike, and orders of magnitude below the smallest
// realistic wei-denominated figure (1 gwei = 1e9 wei), so it separates "an
// absurdly generous ceiling" from "a wei value in a gwei field" cleanly.
var maxPlausibleGasCeilingGwei = decimal.NewFromInt(1_000_000)

func (p SweepPolicy) Validate() error {
	if err := ValidateAmountMagnitude("sweep policy", "min_threshold", p.MinThreshold); err != nil {
		return err
	}
	if err := ValidateAmountMagnitude("sweep policy", "gas_ceiling", p.GasCeiling); err != nil {
		return err
	}
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
	// A gwei ceiling above maxPlausibleGasCeilingGwei is not a ceiling
	// anyone means: it is a wei-denominated figure pasted into a gwei field,
	// which disables the gate entirely (G-M3). Turn the unit mismatch that
	// used to be a documentation contradiction into a startup rejection
	// (working-agreements §5: prefer a machine check over a comment).
	if p.GasCeiling.GreaterThan(maxPlausibleGasCeilingGwei) {
		return fmt.Errorf("core: sweep policy: gas_ceiling=%s gwei is implausibly high (max %s) -- it looks like a wei value in a gwei field, which would disable the gas gate entirely; see SweepPolicy.GasCeiling: %w",
			p.GasCeiling.String(), maxPlausibleGasCeilingGwei.String(), ErrInvalidInput)
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
	// ReorgPolicyManual (the default) does not reverse anything on its own:
	// a human decides whether and how to reverse the booking's journal,
	// after verifying against a second source (RUNBOOK §12).
	//
	// It is deliberately NOT "alert only". Alerting alone is what this
	// policy used to do, and it meant the whole verdict lived in a log line
	// plus a counter increment, from a recheck that went quiet as soon as
	// the booking's block fell outside the recheck window -- on a
	// 2-second chain, minutes. So under the DEFAULT policy the answer to
	// "which booking do I look at" expired before an on-call engineer could
	// use it (G-M8). Every detection now also opens a durable DepositReorg
	// row that outlives the window and the process, and only an operator's
	// explicit resolution takes it off that queue.
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
