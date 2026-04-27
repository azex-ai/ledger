// Package channel defines the inbound webhook contract used by the
// ledger HTTP server to delegate signature verification and payload
// parsing to a per-channel Adapter (e.g. on-chain block scanner, bank
// callback, payment-processor webhook).
//
// Adapter implementations live in subpackages (channel/onchain for EVM,
// future channel/bank, channel/tron, ...). Each Adapter is responsible
// for its own authenticity check; the HTTP layer trusts an Adapter only
// to the extent that the channel name in the URL matches the channel
// the booking was created against.
package channel
