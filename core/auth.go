// Package core: auth.go
//
// Per-journal authorization signing (docs/plans/2026-08-21-tamper-evident-ledger-design.md
// §7, P5 of the integrity-hardening wave). This file defines:
//
//   - the canonical, deterministic byte encoding a posting intent is reduced
//     to before it is handed to a core.Attestor (CanonicalJournalDigest /
//     EncodeAmount) -- pure functions, no DB access, no KMS call, so they are
//     always safe to run inside OR outside a transaction on their own; it is
//     the caller's job (postgres.LedgerStore.PostJournal) to make sure the
//     Attestor.Sign call this digest feeds into never happens while a DB
//     transaction is open (financial.md);
//   - the policy knobs the design doc explicitly left unpicked (§14 items
//     1-2; item 3 -- the withdrawal-gate threshold -- is a separate,
//     not-yet-wired release, see WithdrawalSignatureThreshold below) as
//     injectable configuration whose zero value is a startup/wiring error,
//     never a silent default (M3.1 secure-by-default precedent,
//     docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9.2 addendum).
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
// Policy: the questions design doc §14 leaves explicitly unpicked
// ---------------------------------------------------------------------------

// AttestorFailureMode governs what postgres.LedgerStore.PostJournal does
// when the configured Attestor cannot produce a signature (KMS unreachable,
// throttled, network partition, ...) for a posting that AuthPolicy.Coverage
// requires to be signed.
//
// AttestorFailureModeUnset is deliberately the zero value: it is NOT
// "fail-open" or "fail-closed", it is "nobody decided", and
// AuthPolicy.ValidateFailureMode refuses it -- exactly like
// TokenConfig.AutoCreditCeiling's secure-by-default fence
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9.2 addendum,
// M3.1). Aaron picks a mode once the write-latency impact of a synchronous
// KMS round trip has been measured (design doc §11 / §14 item 1); this
// package never guesses on his behalf.
type AttestorFailureMode int

const (
	AttestorFailureModeUnset AttestorFailureMode = iota
	// AttestorFailureModeFailClosed rejects the posting (PostJournal returns
	// an error wrapping ErrAttestorUnavailable) when the Attestor cannot
	// sign a journal AuthPolicy.Coverage marks as requiring a signature.
	// Correct on the money-path; makes the Attestor an availability
	// dependency of the ledger's write path for covered journal types.
	AttestorFailureModeFailClosed
	// AttestorFailureModeFailOpen posts the journal with empty
	// auth_digest/auth_signature/auth_key_id when the Attestor cannot sign,
	// preserving write availability. Nothing in this phase (P5) reads that
	// empty state to block anything -- the withdrawal gate that would is a
	// deliberately separate, later release (see
	// WithdrawalSignatureThreshold's doc comment) -- so choosing this mode
	// today means unsigned journals accumulate with no consumer yet
	// refusing them.
	AttestorFailureModeFailOpen
)

// SignatureRequirement is AuthPolicy.Coverage's per-journal-type-code
// decision: does a posting of this type need a valid Attestor signature at
// all.
type SignatureRequirement int

const (
	// SignatureRequirementUnset is the zero value ("nobody decided for this
	// code yet"). AuthPolicy.RequirementFor refuses it -- there is no
	// "types not listed are exempt" fallback.
	SignatureRequirementUnset SignatureRequirement = iota
	// SignatureRequirementRequired means PostJournal must attempt to sign
	// postings of this journal-type code; AttestorFailureMode governs what
	// happens if the attempt fails.
	SignatureRequirementRequired
	// SignatureRequirementExempt means PostJournal must NOT attempt to sign
	// postings of this journal-type code (e.g. a reviewed, deliberately
	// non-money-path type) -- a deliberate opt-out, not a default.
	SignatureRequirementExempt
)

// AuthPolicy is the consumer-injected write-path policy for per-journal
// authorization signing. It exists because design doc §14 leaves two
// questions explicitly undecided, and this package refuses to guess either:
//
//   - FailureMode (item 1): validated eagerly, once, when the Attestor is
//     wired (ledger.WithAuthPolicy) -- it does not depend on which journal
//     types exist.
//   - Coverage (item 2): validated lazily, per journal-type CODE, the
//     moment a posting of that type is about to be signed. It cannot be
//     validated eagerly at wiring time the way FailureMode can, because
//     journal types are an open, dynamic set (core.JournalTypeStore.
//     CreateJournalType can add one at any time) rather than a static list
//     known up front the way ChainConfig.CreditTokens is -- RequirementFor
//     is still fail-closed (an undecided code is refused, not treated as
//     exempt), just checked at a different, unavoidably later, point.
type AuthPolicy struct {
	FailureMode AttestorFailureMode
	// Coverage maps journal-type CODE (core.JournalType.Code, not uid --
	// codes are stable and known at configuration time) to whether postings
	// of that type require a signature. A code this ledger instance will
	// actually post that is missing here, or mapped to
	// SignatureRequirementUnset, is a RequirementFor error.
	Coverage map[string]SignatureRequirement
}

// ValidateFailureMode checks the one AuthPolicy question that does not
// depend on which journal types exist. Call it once, eagerly, when wiring
// the Attestor.
func (p AuthPolicy) ValidateFailureMode() error {
	switch p.FailureMode {
	case AttestorFailureModeFailClosed, AttestorFailureModeFailOpen:
		return nil
	default:
		return fmt.Errorf("core: auth policy: FailureMode must be explicitly AttestorFailureModeFailClosed or AttestorFailureModeFailOpen (design doc §14 item 1), got %d: %w", p.FailureMode, ErrInvalidInput)
	}
}

// RequirementFor returns the signature requirement for journalTypeCode, or
// an error if AuthPolicy.Coverage has no explicit decision for it. See the
// AuthPolicy doc comment for why this check happens here (lazily) rather
// than at wiring time.
func (p AuthPolicy) RequirementFor(journalTypeCode string) (SignatureRequirement, error) {
	req, ok := p.Coverage[journalTypeCode]
	if !ok || req == SignatureRequirementUnset {
		return SignatureRequirementUnset, fmt.Errorf(
			"core: auth policy: journal type %q has no signature-coverage decision (design doc §14 item 2); "+
				"set AuthPolicy.Coverage[%q] to SignatureRequirementRequired or SignatureRequirementExempt before posting this type: %w",
			journalTypeCode, journalTypeCode, ErrInvalidInput)
	}
	return req, nil
}

// WithdrawalGateDisabled is the explicit sentinel a consumer sets
// WithdrawalSignatureThreshold.MinSignedAmount to when they deliberately
// want no withdrawal-side signature gate at all -- distinct from the zero
// value, which "nobody decided" and is refused, mirroring
// core.UnboundedAutoCredit's precedent
// (docs/plans/2026-07-11-crypto-deposit-sweep-design.md §9.2 addendum).
var WithdrawalGateDisabled = decimal.NewFromInt(-1)

// WithdrawalSignatureThreshold is the configuration surface for design doc
// §14 item 3 (the withdrawal-gate threshold) and §12's P5 row: "提现门要求签名是行为变更，单独
// release" -- requiring a signature before letting funds leave the system
// is a deliberate, separate, later release, not part of P5. Nothing in this
// package or postgres.LedgerStore reads this type yet.
//
// It is defined now, with a startup-error zero value (mirroring
// core.TokenConfig.AutoCreditCeiling), purely so that later release does
// not have to invent the configuration shape from scratch under pressure,
// and so nobody wires a "0 = no gate" default when it lands -- MinSignedAmount
// left at zero is refused by Configured(), never silently treated as "off".
type WithdrawalSignatureThreshold struct {
	// MinSignedAmount is the minimum withdrawal amount, at or above which
	// every journal contributing to the withdrawable balance must carry a
	// valid signature. Set to WithdrawalGateDisabled to explicitly opt out.
	MinSignedAmount decimal.Decimal
}

// Configured reports whether MinSignedAmount has been deliberately set:
// either to a positive threshold, or to the explicit WithdrawalGateDisabled
// sentinel.
func (t WithdrawalSignatureThreshold) Configured() bool {
	return t.MinSignedAmount.IsPositive() || t.MinSignedAmount.Equal(WithdrawalGateDisabled)
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
