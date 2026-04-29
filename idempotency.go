package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewIdempotencyKey generates an idempotency key in the form "<scope>:<16-byte-hex>".
// The random suffix is produced by crypto/rand so it is safe for concurrent use
// and free from timestamp collisions.
//
// Convention:
//
//	key := ledger.NewIdempotencyKey("deposit")
//	// e.g. "deposit:a3f82c1d4b7e9f0011223344aabbccdd"
func NewIdempotencyKey(scope string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is a system-level error (e.g. exhausted entropy pool).
		// Panic is appropriate here — the process cannot generate safe random IDs.
		panic(fmt.Sprintf("ledger: NewIdempotencyKey: crypto/rand failed: %v", err))
	}
	return scope + ":" + hex.EncodeToString(b[:])
}
