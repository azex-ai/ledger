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
// NUMERIC(30,18): precision 30 with scale 18 leaves 12 integer digits, so
// the largest value is just under 10^12, and rescaling it to 18 fractional
// digits gives an integer just under 10^30 -- about 100 bits, with headroom
// inside 128. (The old wording said "10^30 scaled by 10^18 needs ~100
// bits", which multiplied the scaling in twice; the conclusion was right,
// the derivation was not -- 2026-09-02 audit, A-N7.) The width itself is
// part of the wire contract -- changing it is a breaking encoding change
// exactly like bumping the domain separator (see authDigestDomain).
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
	// Before the Exp below. The `shift > 0` branch computes
	// 10^(18+exponent) as a big.Int, which for a positive exponent of any
	// size is unbounded work -- and the range check that used to be the
	// only guard here (bigIntToFixedTwosComplement) runs AFTER it, on the
	// result it never gets (R-4, 2026-09-04 recheck).
	if err := validateAmountIsRescalable("encode amount", "amount", amt); err != nil {
		return nil, err
	}
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
// Canonical timestamp encoding
// ---------------------------------------------------------------------------

// canonicalTimestamp normalizes t to the exact instant this package's
// digests encode AND the exact instant a TIMESTAMPTZ column can ever
// persist: UTC, floored (never rounded) to microsecond resolution -- the
// same floor pgx's binary Timestamptz encoder performs when writing a Go
// time.Time to Postgres (ts.Time.Nanosecond()/1000, integer division; see
// jackc/pgx/v5/pgtype/timestamptz.go's encodePlanTimestamptzCodecBinary).
//
// Without this, CanonicalJournalDigest/encodeAttestedEntry signed over
// whatever sub-microsecond digits the caller's in-memory time.Time
// happened to carry -- fine on macOS, where time.Now() already returns
// microsecond-aligned values, so the bug was invisible in local dev; wrong
// on Linux (production), which has genuine nanosecond clock resolution: a
// digest signed over the true nanosecond instant can never match the
// digest a verifier recomputes from that same instant AFTER it has been
// stored in and read back from a TIMESTAMPTZ column, because storage
// itself already discarded those digits. Every downstream verifier
// (VerifyJournalAuth via JournalAuthMaterial, VerifyLedger, RunAttestBatch's
// T4 verdict cache) can only ever reconstruct effectiveAt by reading it
// back from the DB -- so on any deployment with real sub-microsecond clock
// resolution, every signed-then-verified journal failed. Applying this
// floor before encoding makes the digest depend only on the instant
// Postgres actually stores, on every platform.
//
// No new domain separator accompanies this fix (contrast authDigestDomain's
// V1->current bump): for any effectiveAt that was already microsecond-
// aligned -- every instant that has round-tripped through a TIMESTAMPTZ
// column, and every instant macOS's time.Now() ever produced -- this floor
// is a no-op, so the digest bytes are unchanged and every signature that
// ever verified successfully keeps verifying. For an effectiveAt with a
// genuine nanosecond remainder (only reachable pre-fix on a platform with
// sub-microsecond clock resolution, i.e. Linux/production), VerifyJournalAuth
// was already failing before this change -- there is no passing verification
// state this fix could invalidate. A domain separator exists to mark "old
// bytes mean something different now"; here old bytes for the only case
// that ever verified mean exactly the same thing.
func canonicalTimestamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// ---------------------------------------------------------------------------
// Canonical journal digest
// ---------------------------------------------------------------------------

// authDigestDomain domain-separates CanonicalJournalDigest's output from
// any other hash this library might ever compute over similarly-shaped
// data. A breaking encoding change (field added/removed/reordered, width
// changed) MUST introduce a new domain separator -- never reuse this byte
// for an incompatible layout (design doc §7.2 / §12).
//
// Domain separator allocation across the package (contracts §2.6, Team
// Lead, 2026-08-21 -- a cross-task shared resource, same class as
// migration numbers / .sql files / reconcile check names):
//   - 0x00 / 0x01: reserved by RFC 6962 (leaf / internal node), external
//     spec, permanent. This also means the retired auth digest V1 (below)
//     can never be reused -- it already burned 0x01.
//   - 0x02 / 0x03: core/attestation.go's batchDigestDomain / root hash
//     (P6, merged). Do not touch.
//   - 0x10: this constant (journal auth digest).
//   - 0x11: core/reservation_auth.go's reservationDischargeDigestDomain
//     (signed reservation discharge claims, remediation contract §7.18 /
//     docs/INVARIANTS.md I-65). Do not touch.
//
// V1 (0x01, retired) included input.EventUID in the digest, which board
// #12/#13's RunInTx signing-gap fix discovered cannot be known at
// Authorize time for an event-linked journal composed via
// booker.Transition (the event uid is minted inside the transaction that
// follows). Signing with EventUID="" and later attaching the real event
// uid for the FK link, without re-signing, would have made every such
// journal fail VerifyJournalAuth's recomputation the moment a verifier
// reconstructed input from the persisted (real-EventUID) row -- a false
// positive indistinguishable from an actual forgery. Team Lead's ruling
// (2026-08-21): remove EventUID from the digest entirely rather than
// carry two digest shapes plus a discriminator. No journal was ever
// signed under V1 in a real deployment (no external users yet), so
// bumping the domain separator was the cheapest possible time to do this.
const authDigestDomain = byte(0x10)

// CanonicalJournalDigest computes the deterministic, domain-separated
// SHA-256 digest of a posting intent, using only input's uid-space fields
// (never internal storage ids) plus the already-resolved effectiveAt the
// caller intends to persist. It is a pure function: no DB access, no KMS
// call -- see the package doc comment for why that matters.
//
// input.EventUID is deliberately NOT part of this digest. It is
// provenance metadata (which event caused this posting), not posting
// intent (who/when/which accounts/how much/idempotency key/reversal) --
// and the event a journal links to is not always known at Authorize time
// (see authDigestDomain's doc comment on why V1 included it and had to
// be retired). The event/journal link remains a DB-structural guarantee
// (I-10: same-transaction write, plus 045's set-once FK on
// journals.event_id), never a cryptographic one -- signing could not add
// atomicity to that link anyway.
//
// input.Metadata is ALSO deliberately not part of this digest, for a
// simpler reason: it is caller-supplied free-form annotation (e.g.
// PendingStore's "reason" key on a cancellation), never posting intent a
// verifier needs to recompute, and folding an unbounded map into a
// fixed-layout byte encoding would need its own canonicalization (sorted
// keys, escaping) this function does not otherwise require. Practical
// consequence: VerifyJournalAuth's signature check says nothing about
// whether a journal's stored metadata matches what was signed.
//
// Who can actually exploit that, precisely (corrected 2026-09-02, audit
// C-m5 -- the previous wording said "a party who can write to the journals
// table directly", which overstated it and invited a maintainer to
// re-litigate a decision that is already covered): NOT the application
// credential. `ledger_journals_block_arbitrary_update` (migration 001)
// rejects any UPDATE on journals whose mutable-column whitelist is exactly
// ['event_id'], so ledger_app cannot alter metadata on an existing row at
// all. It takes ledger_owner or a superuser -- the same party that can drop
// the trigger, rewrite the whole table, or recompute a self-consistent
// attestation chain, i.e. the party P6's external anchor exists to catch
// rather than the one P5's signature does.
//
// The fields this digest does not cover have been enumerated and are
// exactly two: EventUID (provenance, disclosed in I-26) and Metadata.
// Neither affects accounting outcome (accounts, amounts, journal type,
// idempotency key, reversal linkage) nor withdrawal eligibility, so
// widening the byte layout to include them would add encoding risk for no
// gain.
//
// Byte layout (all integers big-endian, unsigned unless noted):
//
//	SHA-256(
//	  0x10                                        -- domain separator
//	  LP(input.JournalTypeUID)
//	  LP(input.IdempotencyKey)
//	  BE64(input.ActorID)                         -- bit pattern of the int64
//	  LP(input.Source)
//	  LP(canonicalTimestamp(effectiveAt).Format(RFC3339Nano))
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
	buf.WriteByte(authDigestDomain)

	writeLenPrefixed(&buf, input.JournalTypeUID)
	writeLenPrefixed(&buf, input.IdempotencyKey)
	writeBE64(&buf, uint64(input.ActorID))
	writeLenPrefixed(&buf, input.Source)
	writeLenPrefixed(&buf, canonicalTimestamp(effectiveAt).Format(time.RFC3339Nano))
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
// RunInTx gap fix (design doc §7.5) -- pre-authorization
// ---------------------------------------------------------------------------
//
// §7.2 put the Attestor call outside any DB transaction, but never said how
// that composes with RunInTx: the caller-owned transaction it wraps has
// already begun by the time a RunInTx callback runs, so there is no safe
// point left inside it to call an Attestor without violating financial.md.
// PostJournal's tx-mode branch therefore always posted unsigned -- silently,
// for every journal composed with a caller's own writes via RunInTx,
// including the deposit-confirmation journal P5 exists to protect
// (service/onchain.go's postDepositConfirmedJournal).
//
// The fix: split signing into two calls the caller sequences itself.
// Authorize (LedgerStore.Authorize / ledger.Service.Authorize) runs BEFORE
// opening any transaction -- it may call the Attestor. PostAuthorized
// (LedgerStore.PostAuthorized) runs the actual write, from either pool mode
// or a caller-owned transaction (RunInTx), using only the already-computed
// AuthorizedJournal -- it never touches the Attestor, so it is always safe
// to call from inside RunInTx.

// AuthStatus records WHY a journal's auth_digest/auth_signature/auth_key_id
// are (or are not) populated. Persisted verbatim in journals.auth_status
// (migration 051) so "why wasn't this signed" is a queryable fact instead
// of something a reader has to infer from three possibly-empty byte
// columns -- indistinguishability here was exactly the bug (design doc
// §7.5): before this column existed, "no Attestor configured" and "posted
// via a transaction with no safe point to sign" were the same observable
// state (all three columns empty), so a verifier could not tell "this
// deployment never turns on signing" apart from "this specific write path
// skipped it even though signing is on".
type AuthStatus string

const (
	// AuthStatusSigned: an Attestor was configured and this journal carries
	// a valid signature over its canonical digest.
	AuthStatusSigned AuthStatus = "signed"
	// AuthStatusUnsignedNoAttestor: no Attestor is configured for this
	// deployment at all -- the signing feature is off system-wide. Every
	// journal posted before P5 (migration 046) predates the column
	// entirely and is backfilled to this value unless it already carries a
	// signature (migration 051's up.sql).
	AuthStatusUnsignedNoAttestor AuthStatus = "unsigned_no_attestor"
	// AuthStatusUnsignedTxMode: this journal was posted through a write
	// path with no safe point to call an Attestor without violating
	// financial.md's "no external calls inside a transaction" rule -- a
	// store bound via WithDB (i.e. any JournalWriter call composed inside
	// ledger.Service.RunInTx, including PostJournal's tx-mode branch,
	// ExecuteTemplateBatch, ReverseJournal, and ReverseJournalFraction all
	// running that way) -- and the caller did not go through
	// Authorize/PostAuthorized (or, for a reversal, AuthorizeReversal) to
	// close that gap for this specific posting. Board #15 (W2-T1) closed
	// this gap for ExecuteTemplateBatch/ReverseJournal/ReverseJournalFraction
	// specifically in POOL mode (they self-manage their own transaction,
	// the same structural opportunity PostJournal's pool-mode branch always
	// had): those three now sign under a configured Attestor in pool mode
	// and only fall back to this status in genuine tx mode.
	AuthStatusUnsignedTxMode AuthStatus = "unsigned_tx_mode"
)

// AuthorizedJournal is the result of pre-authorizing a JournalInput outside
// any database transaction: the canonical uid-space digest (§7.2) and
// whatever an Attestor produced for it, packaged so PostAuthorized can
// persist it without calling the Attestor a second time. Obtained via
// LedgerStore.Authorize / ledger.Service.Authorize; callers must not
// construct one by hand outside those two functions (the zero value's
// Status is "", which PostAuthorized rejects rather than silently treating
// as any of the three real states -- fail-closed, same reasoning as
// core.CheckResult.Complete's zero value).
//
// EventUID: CanonicalJournalDigest never covers it (see that function's
// doc comment and authDigestDomain's), so a caller MAY freely set or
// change Input.EventUID after Authorize returns -- e.g. once
// booker.Transition mints the real event uid inside the transaction this
// pre-authorization exists to get into -- without invalidating Digest or
// needing to re-sign. The event/journal link is a DB-structural guarantee
// (I-10: same-transaction write + 045's set-once FK), never a
// cryptographic one; per-journal signing's defense against M5 (a forger
// without Attestor access cannot produce a valid signature for ANY
// shape) does not depend on which event a signed journal links to.
type AuthorizedJournal struct {
	Input       JournalInput
	EffectiveAt time.Time
	Digest      []byte
	Signature   []byte
	KeyID       string
	Status      AuthStatus
}

// JournalAuthMaterial is everything core.VerifyJournalAuth needs for one
// journal: its reconstructed uid-space JournalInput (Entries populated),
// the effective-at timestamp it was signed under, and the stored
// digest/signature/keyID to check against. Batch-fetched by
// postgres.AttestationStore.JournalAuthMaterial (design doc §4.5's
// batched-fetch recommendation, T4) for two callers that both need the
// identical per-journal answer: service.AttestationService.RunAttestBatch
// (computing a fresh JournalAuthVerdict at attestation time) and
// service.VerifyLedger (recomputing AuthVerdictDigest from live data to
// detect drift) -- defined once here rather than twice.
type JournalAuthMaterial struct {
	Input         JournalInput
	EffectiveAt   time.Time
	AuthDigest    []byte
	AuthSignature []byte
	AuthKeyID     string
	// AuthStatus is the journal's stored journals.auth_status. It is NOT an
	// input to VerifyJournalAuth (which deliberately answers only "does this
	// signature check out", see its scope note) -- it is here so a caller
	// that gets a NEGATIVE answer can tell the three unsigned cases apart:
	//   - AuthStatusUnsignedTxMode: legitimate (posted inside a caller's
	//     transaction, where there was no safe point to sign), and must not
	//     be reported as tamper evidence.
	//   - AuthStatusUnsignedNoAttestor: signing was off system-wide -- or the
	//     row never went through PostJournal at all, which is what a forged
	//     INSERT looks like, since this is auth_status's column default.
	//   - AuthStatusSigned but failing verification: real tamper evidence.
	// service.VerifyLedger's uncovered-entry check (design doc §8.4 step 3)
	// needs exactly this distinction; without it, every journal a consumer
	// legitimately posted via RunInTx would read as a forgery.
	AuthStatus AuthStatus
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

// JournalInputFromRecord reassembles the uid-space JournalInput
// CanonicalJournalDigest/VerifyJournalAuth need from a persisted Journal
// and its Entries -- the shape any caller that fetched a journal via a
// QueryProvider (or an equivalent store) already has on hand. EventUID is
// deliberately left unset: it is not part of the canonical digest (see
// authDigestDomain's doc comment), so including it here would do nothing
// but invite a future caller to assume otherwise.
func JournalInputFromRecord(j Journal, entries []Entry) JournalInput {
	input := JournalInput{
		JournalTypeUID: j.JournalTypeUID,
		IdempotencyKey: j.IdempotencyKey,
		ActorID:        j.ActorID,
		Source:         j.Source,
		ReversalOfUID:  j.ReversalOfUID,
		Entries:        make([]EntryInput, len(entries)),
	}
	for i, e := range entries {
		input.Entries[i] = EntryInput{
			AccountHolder:     e.AccountHolder,
			CurrencyUID:       e.CurrencyUID,
			ClassificationUID: e.ClassificationUID,
			EntryType:         e.EntryType,
			Amount:            e.Amount,
		}
	}
	return input
}
