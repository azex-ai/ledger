// Package evm is the EVM chain-family adapter for the ledger's crypto
// deposit + sweep bundle (docs/plans/2026-07-11-crypto-deposit-sweep-design.md,
// .team/context/foundation-contract.md §5). It implements the RPC-facing
// ports core/service defines against a real chain: watching ERC-20 Transfer
// logs for registered deposit addresses, scanning balances for the sweep
// job, submitting factory.batchSweep transactions, and a default local
// private-key Signer.
//
// This module intentionally depends on github.com/ethereum/go-ethereum --
// the root ledger module must never import this package or go-ethereum
// (design doc §1: "不用 crypto 的消费方根 go.mod 不变，不拉 geth"). Consumers
// that want on-chain crypto deposits import
// github.com/azex-ai/ledger/chains/evm explicitly in their own composition
// root and wire its exported constructors against the core ports.
//
// Everything here is a pure adapter: no business decisions (confirmation
// thresholds, sweep policy, reorg policy) are made in this package -- those
// live in core/service and are injected in via core.ChainSet / core.SweepPolicy
// / core.ReorgPolicy. This package only translates between RPC/ABI bytes and
// the domain types in core/onchain.go.
package evm
