// Package core: auth.go
//
// Per-journal authorization signing (docs/plans/2026-08-21-tamper-evident-ledger-design.md
// §7, P5 of the integrity-hardening wave). This file defines the canonical,
// deterministic byte encoding a posting intent is reduced to before it is
// handed to a core.Attestor (CanonicalJournalDigest / EncodeAmount) --
// pure functions, no DB access, no KMS/signing call, so they are always
// safe to run inside OR outside a transaction on their own; it is the
// caller's job (postgres.LedgerStore.PostJournal) to make sure the
// Attestor.Sign call this digest feeds into never happens while a DB
// transaction is open (financial.md) -- and, per Team Lead's 2026-08-21
// simplification pass, benchmarking confirmed that call is purely additive
// latency (it runs before any advisory lock is taken), not lock-extending.
//
// Scope (deliberately narrow, per Team Lead's correction of the original
// task brief): every journal is signed whenever an Attestor is configured
// -- there is no per-journal-type coverage decision and no
// KMS-failure-mode branch. A local in-process key already satisfies this
// wave's threat model (design doc §1 non-goal 2: "app + KMS 同时失陷" is
// explicitly not defended against, for ANY key custody model, local or
// remote) -- so the extra configuration surface the original brief asked
// for was solving a deployment problem (remote KMS latency/availability)
// this project does not have. If a future deployment DOES put the
// Attestor behind a flaky remote call, retry/degrade semantics belong
// inside that Attestor implementation, not as a policy knob on this port.
package core

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// ---------------------------------------------------------------------------
// Canonical amount encoding
// ---------------------------------------------------------------------------

// amountScale is the fixed number of fractional decimal digits every amount
// is rescaled to before encoding. NUMERIC(30,18) is the widest amount column
// in this schema (core.CurrencyInput.Validate enforces every currency's
// Exponent into [0,18]), so 18 is the ceiling.
const amountScale = 18

// authAmountEncodedLen is the fixed width, in bytes, EncodeAmount always
// produces. 16 bytes (128-bit two's complement) comfortably covers
// NUMERIC(30,18)'s maximum magnitude (10^30 scaled by 10^18 needs ~100 bits)
// with headroom; the width itself is part of the wire contract -- changing
// it is a breaking encoding change exactly like bumping the domain
// separator (see authDigestDomainV1).
const authAmountEncodedLen = 16

// EncodeAmount deterministically encodes amt as a fixed-point integer scaled
// to exactly amountScale (18) decimal places, then serializes that integer
// as a fixed authAmountEncodedLen-byte big-endian two's complement value.
//
// This exists because decimal.Decimal.String() is NOT a stable wire format:
// trailing zeros, and the coefficient/exponent pair a Decimal happens to be
// constructed with, can vary between two values that are mathematically
// equal -- a canonical digest that a KMS key signs must depend only on the
// value, never on incidental representation. Negative zero and positive
// zero encode identically (big.Int has no observable negative zero through
// its public Sign()/Cmp()/Bytes() API, which is all this function uses).
//
// Returns core.ErrInvalidInput if amt carries more than 18 significant
// fractional digits (rescaling would lose precision) or its scaled
// magnitude does not fit in a signed 128-bit integer.
func EncodeAmount(amt decimal.Decimal) ([]byte, error) {
	coeff := amt.Coefficient() // value = coeff * 10^exp; copy before mutating below.
	exp := amt.Exponent()

	scaled := new(big.Int).Set(coeff)
	shift := int64(amountScale) + int64(exp)
	switch {
	case shift > 0:
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(shift), nil)
		scaled.Mul(scaled, pow)
	case shift < 0:
		pow := new(big.Int).Exp(big.NewInt(10), big.NewInt(-shift), nil)
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(scaled, pow, r)
		if r.Sign() != 0 {
			return nil, fmt.Errorf("core: encode amount: %s has more than %d fractional digits: %w", amt.String(), amountScale, ErrInvalidInput)
		}
		scaled = q
	}

	return bigIntToFixedTwosComplement(scaled, authAmountEncodedLen)
}

// bigIntToFixedTwosComplement encodes v as a big-endian two's complement
// integer padded (or rejected) to exactly size bytes.
func bigIntToFixedTwosComplement(v *big.Int, size int) ([]byte, error) {
	max := new(big.Int).Lsh(big.NewInt(1), uint(size*8-1)) // 2^(size*8-1)
	min := new(big.Int).Neg(max)                           // -2^(size*8-1)
	if v.Cmp(max) >= 0 || v.Cmp(min) < 0 {
		return nil, fmt.Errorf("core: encode amount: scaled value out of range for %d-byte two's complement: %w", size, ErrInvalidInput)
	}

	out := make([]byte, size)
	if v.Sign() >= 0 {
		b := v.Bytes()
		copy(out[size-len(b):], b)
		return out, nil
	}

	// Two's complement of a negative value: 2^(size*8) + v.
	mod := new(big.Int).Lsh(big.NewInt(1), uint(size*8))
	tc := new(big.Int).Add(mod, v)
	b := tc.Bytes()
	copy(out[size-len(b):], b)
	return out, nil
}

// ---------------------------------------------------------------------------
// Canonical journal digest
// ---------------------------------------------------------------------------

// authDigestDomainV1 domain-separates CanonicalJournalDigest's output from
// any other hash this library might ever compute over similarly-shaped
// data. A breaking encoding change (field added/removed/reordered, width
// changed) MUST introduce a new domain separator -- never reuse this byte
// for an incompatible layout (design doc §7.2 / §12).
const authDigestDomainV1 = byte(0x01)

// CanonicalJournalDigest computes the deterministic, domain-separated
// SHA-256 digest of a posting intent, using only input's uid-space fields
// (never internal storage ids) plus the already-resolved effectiveAt the
// caller intends to persist. It is a pure function: no DB access, no KMS
// call -- see the package doc comment for why that matters.
//
// Byte layout (all integers big-endian, unsigned unless noted):
//
//	SHA-256(
//	  0x01                                        -- domain separator
//	  LP(input.JournalTypeUID)
//	  LP(input.IdempotencyKey)
//	  BE64(input.ActorID)                         -- bit pattern of the int64
//	  LP(input.Source)
//	  LP(input.EventUID)                           -- "" if not event-driven
//	  LP(effectiveAt.UTC().Format(RFC3339Nano))
//	  LP(input.ReversalOfUID)                      -- "" for original journals
//	  BE64(len(entries))
//	  for each entry, sorted by (AccountHolder, CurrencyUID,
//	                             ClassificationUID, EntryType, Amount):
//	    BE64(entry.AccountHolder)
//	    LP(entry.CurrencyUID)
//	    LP(entry.ClassificationUID)
//	    LP(string(entry.EntryType))
//	    EncodeAmount(entry.Amount)                 -- 16 bytes, see above
//	)
//
// where LP(s) = BE32(len(utf8 bytes of s)) || utf8 bytes of s. Entries are
// sorted before hashing (a copy of input.Entries; the caller's slice is
// never mutated) so entry order in the caller's slice never changes the
// digest -- callers that build entries by iterating a map, for instance,
// still get a stable signature.
//
// Golden vectors pinning this exact byte layout live in auth_test.go;
// diverging from them is a breaking change requiring a new domain
// separator, not a silent fix.
func CanonicalJournalDigest(input JournalInput, effectiveAt time.Time) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte(authDigestDomainV1)

	writeLenPrefixed(&buf, input.JournalTypeUID)
	writeLenPrefixed(&buf, input.IdempotencyKey)
	writeBE64(&buf, uint64(input.ActorID))
	writeLenPrefixed(&buf, input.Source)
	writeLenPrefixed(&buf, input.EventUID)
	writeLenPrefixed(&buf, effectiveAt.UTC().Format(time.RFC3339Nano))
	writeLenPrefixed(&buf, input.ReversalOfUID)

	entries := make([]EntryInput, len(input.Entries))
	copy(entries, input.Entries)
	sort.Slice(entries, func(i, j int) bool { return entryDigestLess(entries[i], entries[j]) })

	writeBE64(&buf, uint64(len(entries)))
	for i, e := range entries {
		writeBE64(&buf, uint64(e.AccountHolder))
		writeLenPrefixed(&buf, e.CurrencyUID)
		writeLenPrefixed(&buf, e.ClassificationUID)
		writeLenPrefixed(&buf, string(e.EntryType))
		amtBytes, err := EncodeAmount(e.Amount)
		if err != nil {
			return nil, fmt.Errorf("core: canonical journal digest: entry[%d]: %w", i, err)
		}
		buf.Write(amtBytes)
	}

	sum := sha256.Sum256(buf.Bytes())
	return sum[:], nil
}

func entryDigestLess(a, b EntryInput) bool {
	if a.AccountHolder != b.AccountHolder {
		return a.AccountHolder < b.AccountHolder
	}
	if a.CurrencyUID != b.CurrencyUID {
		return a.CurrencyUID < b.CurrencyUID
	}
	if a.ClassificationUID != b.ClassificationUID {
		return a.ClassificationUID < b.ClassificationUID
	}
	if a.EntryType != b.EntryType {
		return a.EntryType < b.EntryType
	}
	return a.Amount.Cmp(b.Amount) < 0
}

func writeLenPrefixed(buf *bytes.Buffer, s string) {
	b := []byte(s)
	writeBE32(buf, uint32(len(b)))
	buf.Write(b)
}

func writeBE32(buf *bytes.Buffer, v uint32) {
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], v)
	buf.Write(tmp[:])
}

func writeBE64(buf *bytes.Buffer, v uint64) {
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], v)
	buf.Write(tmp[:])
}

// ---------------------------------------------------------------------------
// Verification
// ---------------------------------------------------------------------------

// VerifyJournalAuth recomputes input's canonical digest at effectiveAt,
// checks it against storedDigest (what was actually signed at post time),
// then asks verifier to check signature/keyID against the recomputed
// digest. It is the primitive a downstream gate (withdrawal check,
// reconcile, ledger-cli verify -- none of which are wired by this phase,
// design doc §7.3/§7.4) would call; PostJournal itself never calls it.
//
// Returns an error wrapping ErrUnauthorizedJournal if storedDigest is empty
// (the journal was never signed), does not match the recomputed digest
// (the input/effectiveAt passed in do not match what was actually signed),
// signature/keyID are empty, or verifier rejects them.
//
// Scope note: an empty storedDigest is indistinguishable, from this
// function alone, between "signing was never enabled for this deployment"
// and "this journal should have been signed and was not" (a forgery, or a
// bug). Both cases return the same ErrUnauthorizedJournal. Telling them
// apart requires deployment-level knowledge this function does not have
// (e.g. "signing has been enabled since journal id N" or a cutover
// timestamp) -- that is the downstream gate's job (§7.3/§7.4), not this
// primitive's. Callers that have not enabled signing at all should not be
// calling this function in the first place.
func VerifyJournalAuth(ctx context.Context, verifier AuthVerifier, input JournalInput, effectiveAt time.Time, storedDigest, signature []byte, keyID string) error {
	if len(storedDigest) == 0 {
		return fmt.Errorf("core: verify journal auth: journal has no stored digest: %w", ErrUnauthorizedJournal)
	}
	recomputed, err := CanonicalJournalDigest(input, effectiveAt)
	if err != nil {
		return fmt.Errorf("core: verify journal auth: recompute digest: %w", err)
	}
	if !bytes.Equal(recomputed, storedDigest) {
		return fmt.Errorf("core: verify journal auth: stored digest does not match recomputed digest: %w", ErrUnauthorizedJournal)
	}
	if len(signature) == 0 || keyID == "" {
		return fmt.Errorf("core: verify journal auth: journal has no signature: %w", ErrUnauthorizedJournal)
	}
	if err := verifier.Verify(ctx, recomputed, signature, keyID); err != nil {
		return fmt.Errorf("core: verify journal auth: signature invalid: %w: %w", err, ErrUnauthorizedJournal)
	}
	return nil
}
