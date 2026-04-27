// Package onchain provides demo channel.Adapter implementations for
// on-chain block-scanner callbacks. The current implementation is
// EVMAdapter, an HMAC-SHA256-verified webhook handler that parses
// {tx_hash, booking_id, amount, confirmations, status} payloads.
//
// EVMAdapter requires X-Timestamp and X-Signature headers; the signed
// payload is "<X-Timestamp>.<body>" and timestamps outside ±5 min are
// rejected to defeat replay attacks. The signing key is supplied at
// construction time (configured via EVM_WEBHOOK_SECRET).
package onchain
