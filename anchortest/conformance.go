// Package anchortest is a conformance test suite for any core.Anchor
// implementation (design doc §8.3, P6's external anchor). core.Anchor's
// full contract lives in its doc comment (core/interfaces.go) -- this
// package turns that prose into machine-checkable assertions so a
// consumer's own carrier adapter (an object-lock bucket, a public chain,
// ...; this library deliberately ships none in production form, see
// anchordev's package doc for why) can prove it satisfies the same
// contract anchordev.LocalFileAnchor does, without every implementation
// hand-rolling the same handful of edge cases.
//
// # Usage
//
//	func TestMyS3Anchor_Conformance(t *testing.T) {
//		anchortest.RunConformance(t, func() core.Anchor {
//			return myS3Anchor(bucket, "fixed/test/prefix")
//		})
//	}
//
// newAnchor must return a fresh client bound to the SAME backing store on
// every call -- e.g. closing over a fixed bucket+prefix or file path, the
// way a real writer and a real verifier each construct their own client
// pointed at the same external location. That repeatability is what lets
// this suite exercise design doc §8.3 point 3 ("可独立读取 -- 验证方不
// 经过账本自己的服务就能拿到 head"): see the
// IndependentlyConstructedClientSeesSameState phase below.
//
// The backing store newAnchor points at must be EMPTY the first time
// RunConformance calls it -- pass a fixture (a t.TempDir()-rooted path, a
// fresh test bucket/prefix, ...) nothing else writes to.
//
// The suite runs as ONE ordered scenario, not independent subtests: each
// phase builds on the seq state the previous phase left behind. This
// mirrors how a real caller actually uses an Anchor -- one monotonically
// increasing stream of publishes -- rather than pretending every check can
// restart from a clean slate on a store whose lifecycle this package does
// not own. Running a single phase in isolation (`go test -run .../SomePhase`)
// will fail for reasons unrelated to that phase's own assertion; run the
// whole suite.
//
// # What this suite deliberately does not check
//
// Two things adjacent to core.Anchor's contract are left untested rather
// than silently baked in as if the port required them:
//
//   - Whether Publish ACCEPTS a seq that skips ahead of the current head
//     (e.g. publishing 7 when Head is 2). core.Anchor takes no position:
//     anchordev.LocalFileAnchor rejects anything but Head()+1 or an exact
//     replay, while an adapter that stores one object per seq can accept
//     gaps harmlessly. Either is conformant.
//
//     What this suite DOES check, since the 2026-09-02 audit, is that Head
//     never goes BACKWARDS -- see HeadNeverRegressesOnAnOlderPublish below.
//     That is not a matter of taste: service.VerifyLedger relies on it to
//     tell a benign anchor backlog from a rollback, so an implementation
//     that let Head regress would make an erased anchor read as "catch-up
//     pending" (tamper-evident.md M-3/M-4). This package previously
//     declared the whole area out of scope, which is how an adapter whose
//     Head read a single mutable object's current version passed
//     conformance.
//
//   - Concurrency safety of Publish. Design doc §8.3 describes the
//     intended caller as a local retry queue -- i.e. Publish calls
//     serialized by construction. core.Anchor's doc comment makes no
//     promise about two goroutines calling Publish at once. This suite
//     does not require thread-safety of the implementation under test.
//     (anchordev.LocalFileAnchor happens to provide it via an internal
//     mutex; that is pinned by anchordev's own tests, not this suite.)
//
// # What this suite cannot check by itself
//
// Design doc §8.3 lists three properties a *production* carrier must
// have. Only the third is a live-behavior property a black-box Go test
// can exercise:
//
//  1. "In a place the ledger DB credentials cannot reach" -- a property of
//     WHERE the implementation is deployed (which cloud account, which
//     credentials can reach it), not of what a Go value returns from
//     Publish/Head. No test against an in-process core.Anchor value can
//     prove this; it has to be verified by reading the adapter's own
//     composition-root wiring. See docs/RUNBOOK.md's "Choosing an Anchor
//     carrier" section for the judgment call this leaves to the deployer.
//
//  2. "Written content cannot be changed" (append-only / immutable). This
//     suite's MismatchedReplayErrorsAndDoesNotCorrupt phase verifies the
//     half of this that IS observable through the interface (Publish
//     itself refuses to accept a different value for a seq already
//     recorded). It cannot prove, from the interface alone, that nothing
//     else -- an out-of-band admin console, a bucket-policy change, a
//     second set of credentials -- could mutate the underlying bytes
//     without ever calling Publish.
//
//     An implementation that CAN reach its own carrier out of band (every
//     real one can: it holds the client) may hand that ability to this
//     suite via WithOutOfBandWrite, which unlocks the
//     HeadNeverRegressesAfterAnOutOfBandOlderWrite phase -- the one that
//     actually exercises the attack the audit found (a plain PutObject
//     with the ledger's own token, bypassing Publish entirely). Without
//     the hook that phase is SKIPPED, and reported as skipped: not
//     silently passed (working-agreements.md §3).
//
//  3. "Independently readable, without going through the ledger's own
//     service" -- THIS one the suite exercises directly: see
//     IndependentlyConstructedClientSeesSameState below.
package anchortest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/azex-ai/ledger/core"
)

// Violation is one phase of the conformance suite that failed against a
// given core.Anchor implementation.
type Violation struct {
	// Phase names the check that failed (matches the t.Run subtest name
	// RunConformance uses for the same check).
	Phase string
	// Message describes the mismatch.
	Message string
}

func (v Violation) String() string { return v.Phase + ": " + v.Message }

// phase is one ordered step of the scenario. run returns nil if the
// implementation under test satisfied it, errSkipped if the suite was not
// given what that phase needs, or a descriptive error if the implementation
// violated the contract.
type phase struct {
	name string
	run  func() error
}

// errSkipped is a phase reporting "I could not run", which is neither a pass
// nor a violation. RunConformance surfaces it as a t.Skip (visible in test
// output) and Check omits it from the violation list -- what it must never
// become is an absent, silently-green phase (working-agreements.md §3:
// "didn't run" and "ran and passed" must stay distinguishable).
var errSkipped = errors.New("anchortest: phase skipped")

// options collects the optional capabilities a caller can lend the suite.
type options struct {
	outOfBandWrite func(seq int64, head []byte) error
}

// Option configures RunConformance / Check.
type Option func(*options)

// WithOutOfBandWrite lends the suite the ability to write the carrier
// WITHOUT going through Publish -- exactly what an attacker holding the
// ledger's own storage credential has, and what core.Anchor's method set
// cannot express (the 2026-09-02 audit's M-4: the R2 adapter's Publish
// carefully refused an older seq, and a plain s3.PutObject with the same
// token rolled the head back anyway).
//
// The function must write (seq, head) to the same backing store newAnchor
// points at, using the implementation's native client, in the same encoding
// Publish would produce -- e.g. for an S3-backed anchor, a raw PutObject of
// the object body for that seq. It must NOT call Publish (that would test
// nothing new: HeadNeverRegressesOnAnOlderPublish already covers the Publish
// path).
//
// Supplying it unlocks HeadNeverRegressesAfterAnOutOfBandOlderWrite. An
// implementation whose carrier genuinely cannot be written except through
// Publish may omit it; the phase then reports as skipped.
func WithOutOfBandWrite(write func(seq int64, head []byte) error) Option {
	return func(o *options) { o.outOfBandWrite = write }
}

// phases builds the ordered scenario against a fresh newAnchor() instance.
// Both Check and RunConformance iterate the same list, so there is exactly
// one place the actual assertions live.
func phases(newAnchor func() core.Anchor, opts ...Option) []phase {
	a := newAnchor()
	ctx := context.Background()

	var o options
	for _, opt := range opts {
		opt(&o)
	}

	head1 := fixture(0xA1)
	head2 := fixture(0xB2)
	head2Different := fixture(0xC3)

	return []phase{
		{
			// core.Anchor doc: "Head returns the highest seq the anchor
			// knows about, or 0 if empty."
			name: "HeadOnEmptyReturnsZeroNoError",
			run: func() error {
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head on an anchor nothing has been published to: unexpected error: %w", err)
				}
				if seq != 0 {
					return fmt.Errorf("head on an empty anchor: seq = %d, want 0", seq)
				}
				if len(head) != 0 {
					return fmt.Errorf("head on an empty anchor: head = %x, want empty", head)
				}
				return nil
			},
		},
		{
			// core.Anchor doc: Publish followed by Head must reflect what
			// was published -- the baseline write/read round trip every
			// later phase builds on.
			name: "PublishThenHeadReflectsIt",
			run: func() error {
				if err := a.Publish(ctx, 1, head1); err != nil {
					return fmt.Errorf("Publish(1, ...): unexpected error: %w", err)
				}
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head after Publish(1, ...): unexpected error: %w", err)
				}
				if seq != 1 || !bytes.Equal(head, head1) {
					return fmt.Errorf("head after Publish(1, ...) = (%d, %x), want (1, %x)", seq, head, head1)
				}
				return nil
			},
		},
		{
			// core.Anchor doc: "Head returns the HIGHEST seq the anchor
			// knows about" -- publishing a second, higher seq must move
			// Head forward, not just record it alongside the first.
			name: "HeadReflectsHighestAcrossPublishes",
			run: func() error {
				if err := a.Publish(ctx, 2, head2); err != nil {
					return fmt.Errorf("Publish(2, ...): unexpected error: %w", err)
				}
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head after Publish(2, ...): unexpected error: %w", err)
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head after Publish(2, ...) = (%d, %x), want (2, %x)", seq, head, head2)
				}
				return nil
			},
		},
		{
			// core.Anchor doc: "Publish is idempotent per seq:
			// re-publishing the same seq with identical bytes must
			// succeed."
			name: "IdempotentReplaySucceeds",
			run: func() error {
				if err := a.Publish(ctx, 2, head2); err != nil {
					return fmt.Errorf("idempotent replay of Publish(2, ...) with identical bytes: unexpected error: %w", err)
				}
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head after idempotent replay: unexpected error: %w", err)
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head after idempotent replay = (%d, %x), want (2, %x)", seq, head, head2)
				}
				return nil
			},
		},
		{
			// core.Anchor doc: "...with different bytes must return an
			// error."
			name: "MismatchedReplayErrorsAndDoesNotCorrupt",
			run: func() error {
				err := a.Publish(ctx, 2, head2Different)
				if err == nil {
					return fmt.Errorf("Publish(2, <different bytes>) after Publish(2, head2): expected error, got nil")
				}

				// Not spelled out verbatim in the doc comment, but the
				// only reading of "must return an error" that makes the
				// guarantee mean anything: an error return means the
				// write was REFUSED, not "accepted, but also flagged".
				// An Anchor that errors yet still overwrites its
				// recorded head would be worse than one that silently
				// accepted the mismatch -- it would allow the exact
				// tampering design doc §8.3 point 2 exists to make
				// impossible ("written content cannot be changed") while
				// looking, from the error return alone, like it caught
				// it.
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head after a refused mismatched replay: unexpected error: %w", err)
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head after a refused mismatched replay = (%d, %x), want unchanged (2, %x)", seq, head, head2)
				}
				return nil
			},
		},
		{
			// design doc §8.3 point 3: "可独立读取 -- 验证方不经过账
			// 本自己的服务就能拿到 head". Operationalized as: a second
			// client, constructed from scratch by newAnchor (standing in
			// for a separate verifier process pointed at the same
			// carrier), must see what the first client published -- not
			// just the first client's own in-process view of its own
			// writes.
			name: "IndependentlyConstructedClientSeesSameState",
			run: func() error {
				b := newAnchor()
				seq, head, err := b.Head(ctx)
				if err != nil {
					return fmt.Errorf("head from an independently constructed client: unexpected error: %w", err)
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head from an independently constructed client = (%d, %x), want (2, %x) -- "+
						"state must be persisted on the shared carrier, not held only in the first client's memory",
						seq, head, head2)
				}
				return nil
			},
		},
		{
			// core.Anchor doc: "Head MUST NEVER REGRESS. Once Head has
			// returned seq N, no later call may return a seq lower than
			// N." The half of that observable through Publish: offering an
			// OLDER seq than the current head must not move Head back.
			//
			// Distinct from MismatchedReplayErrorsAndDoesNotCorrupt above,
			// which covers the same seq with different bytes. This one is
			// a DIFFERENT seq -- and it is the property
			// service.VerifyLedger uses to separate "the anchor is behind,
			// catch-up pending" (benign) from "the anchor went backwards"
			// (tamper evidence). An Anchor that let Head regress would make
			// an erased or rolled-back anchor look benign, which is the
			// 2026-09-02 audit's M-3.
			//
			// Whether Publish returns an error or accepts the older seq as
			// a no-op is left to the implementation; what it may not do is
			// let the older value become Head's answer.
			name: "HeadNeverRegressesOnAnOlderPublish",
			run: func() error {
				// Deliberately different bytes for seq 1 than the ones
				// published earlier: an implementation that stores one
				// object per seq should refuse this as a mismatched replay,
				// and one that keeps a single head should refuse it as
				// out-of-order. Either way Head must still be 2.
				_ = a.Publish(ctx, 1, head2Different)
				seq, head, err := a.Head(ctx)
				if err != nil {
					return fmt.Errorf("head after an older Publish attempt: unexpected error: %w", err)
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head after Publish(1, ...) with the current head at seq 2 = (%d, %x), want unchanged (2, %x) -- "+
						"Head must report the HIGHEST seq ever published, not the value of the last write",
						seq, head, head2)
				}
				return nil
			},
		},
		{
			// The same guarantee against the attack that does not use
			// Publish at all (2026-09-02 audit, tamper-evident.md M-4): a
			// raw write to the carrier with the ledger's own credential.
			// Requires WithOutOfBandWrite; skipped, and reported as
			// skipped, without it.
			name: "HeadNeverRegressesAfterAnOutOfBandOlderWrite",
			run: func() error {
				if o.outOfBandWrite == nil {
					return errSkipped
				}
				if err := o.outOfBandWrite(1, head2Different); err != nil {
					return fmt.Errorf("the supplied out-of-band write failed, so this phase could not test anything: %w", err)
				}
				seq, head, err := o.headFrom(newAnchor, ctx)
				if err != nil {
					return err
				}
				if seq != 2 || !bytes.Equal(head, head2) {
					return fmt.Errorf("head after an out-of-band write of an OLDER seq = (%d, %x), want unchanged (2, %x) -- "+
						"a party holding the carrier's write credential must not be able to roll the published head "+
						"backwards; Head has to resolve the highest seq ever published, not the latest write",
						seq, head, head2)
				}
				return nil
			},
		},
	}
}

// headFrom reads Head through a FRESHLY constructed client. After an
// out-of-band write, a client that has been caching (legitimately or not)
// would otherwise answer from its own memory of its own writes, and the
// phase would pass without testing the carrier.
func (o options) headFrom(newAnchor func() core.Anchor, ctx context.Context) (int64, []byte, error) {
	seq, head, err := newAnchor().Head(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("head from a freshly constructed client: unexpected error: %w", err)
	}
	return seq, head, nil
}

// Check runs the full ordered scenario against newAnchor and returns every
// phase that failed (nil if fully conformant). It has no *testing.T
// dependency, which is what lets this package's own tests assert "a
// specific violation was found" without that assertion itself becoming a
// failing top-level `go test` result the way nesting a deliberately
// broken fake under t.Run would (a failing subtest marks every ancestor
// test as failed in Go's testing package, regardless of what the parent
// does with t.Run's returned bool afterward) -- see anchortest's own
// conformance_test.go for exactly that shape.
//
// Most callers should use RunConformance instead; Check is exposed for
// programmatic use (e.g. a health-check endpoint) and for this package's
// self-tests.
func Check(newAnchor func() core.Anchor, opts ...Option) []Violation {
	var violations []Violation
	for _, p := range phases(newAnchor, opts...) {
		err := p.run()
		if err == nil || errors.Is(err, errSkipped) {
			continue
		}
		violations = append(violations, Violation{Phase: p.name, Message: err.Error()})
	}
	return violations
}

// Skipped reports which phases could not run against newAnchor with the
// given options -- the counterpart to Check for a caller that needs to know
// what was NOT covered (a health endpoint reporting "conformant" should not
// do so on the strength of phases that never executed).
func Skipped(newAnchor func() core.Anchor, opts ...Option) []string {
	var skipped []string
	for _, p := range phases(newAnchor, opts...) {
		if err := p.run(); errors.Is(err, errSkipped) {
			skipped = append(skipped, p.name)
		}
	}
	return skipped
}

// RunConformance runs the anchortest conformance suite against a
// core.Anchor implementation, reporting each failed phase as a named
// t.Run subtest (so a failure points at the exact phase, the same way any
// other Go test does). See the package doc for newAnchor's contract (same
// backing store across calls, empty at the start) and for what is and is
// not covered.
func RunConformance(t *testing.T, newAnchor func() core.Anchor, opts ...Option) {
	t.Helper()
	for _, p := range phases(newAnchor, opts...) {
		p := p
		t.Run(p.name, func(t *testing.T) {
			err := p.run()
			switch {
			case err == nil:
			case errors.Is(err, errSkipped):
				t.Skip("this phase needs a capability the caller did not supply (see anchortest's Option functions)")
			default:
				t.Error(err)
			}
		})
	}
}

// fixture returns a 32-byte slice distinguishable by its first byte --
// stands in for a real batch root_hash (design doc §8.3: "几十字节");
// the exact size/content is arbitrary, only distinctness across fixtures
// matters to this suite.
func fixture(first byte) []byte {
	b := make([]byte, 32)
	b[0] = first
	return b
}
