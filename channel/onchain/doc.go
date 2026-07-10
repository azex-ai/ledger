// Package onchain provides demo channel.Adapter implementations for
// on-chain block-scanner callbacks. The current implementation is
// EVMAdapter, an HMAC-SHA256-verified webhook handler.
//
// EVMAdapter requires X-Timestamp and X-Signature headers; the signed
// payload is "<X-Timestamp>.<body>" and timestamps outside ±5 min are
// rejected to defeat replay attacks. The signing key is supplied at
// construction time (configured via EVM_WEBHOOK_SECRET).
//
// Two body shapes are understood:
//   - ParseSighting: {chain_id, tx_hash, txlog_seq, token, from, to, amount,
//     confirmations} -- normalizes into a core.DepositSighting, the shape
//     the crypto-deposit design (docs/plans/2026-07-11-crypto-deposit-sweep-design.md
//     §3) routes to IngestDeposit. This is the path a server prefers when
//     available (see server.sightingParser).
//   - ParseCallback: {tx_hash, booking_uid, amount, confirmations, status} --
//     the legacy shape that transitions a pre-existing booking_uid. Kept for
//     channel.Adapter compliance and any caller still using it directly.
package onchain
