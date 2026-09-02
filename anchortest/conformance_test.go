package anchortest_test

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/azex-ai/ledger/anchortest"
	"github.com/azex-ai/ledger/core"
)

// This file is the suite's own guard rail: for every documented Anchor
// requirement, a deliberately broken fake proves the suite actually
// fails when that requirement is violated, plus one well-behaved fake
// proving the suite is not just permanently red. Without these, a check
// that always passes and a check that actually checks something would be
// indistinguishable (working-agreements.md §3/§5 -- a check that cannot
// fail is not a check).
//
// The four "catches" tests below assert on anchortest.Check's return
// value rather than calling anchortest.RunConformance directly. That is
// a deliberate choice, not a shortcut: Go's testing package marks every
// ancestor test as failed the moment a subtest fails, regardless of what
// the parent does with t.Run's returned bool afterward -- there is no way
// to nest a deliberately-failing RunConformance under t.Run and have the
// enclosing "this correctly caught the bug" test itself report PASS.
// anchortest.Check exists specifically so this package (and any consumer
// who wants the violations without a *testing.T) can inspect the result
// as data. RunConformance itself is a ~10-line loop translating that same
// data into t.Run/t.Error calls; TestRunConformance_PassesWellBehavedImplementation
// below exercises that translation directly, on a case that is expected
// to genuinely pass.

// mustFindViolation fails t (a normal, non-nested assertion -- no
// propagation hazard) unless violations contains one whose Phase matches
// wantPhase.
func mustFindViolation(t *testing.T, violations []anchortest.Violation, wantPhase string) {
	t.Helper()
	for _, v := range violations {
		if v.Phase == wantPhase {
			return
		}
	}
	t.Fatalf("Check did not report a %q violation; got: %v", wantPhase, violations)
}

// -----------------------------------------------------------------------
// fakeIgnoresByteMismatch: violates "re-publishing the same seq with...
// different bytes must return an error" (core.Anchor doc comment). Always
// accepts whatever bytes it's handed for a seq, silently overwriting --
// exactly the failure mode design doc §8.3 point 2 exists to prevent (an
// attacker who can touch the anchor makes their own tampering
// self-consistent).
// -----------------------------------------------------------------------

type fakeIgnoresByteMismatch struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (f *fakeIgnoresByteMismatch) Publish(_ context.Context, seq int64, head []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq = seq
	f.head = append([]byte(nil), head...) // BUG: never checks for a mismatched replay
	return nil
}

func (f *fakeIgnoresByteMismatch) Head(_ context.Context) (int64, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq, f.head, nil
}

func TestCheck_CatchesIgnoresByteMismatch(t *testing.T) {
	shared := &fakeIgnoresByteMismatch{}
	violations := anchortest.Check(func() core.Anchor { return shared })
	mustFindViolation(t, violations, "MismatchedReplayErrorsAndDoesNotCorrupt")
}

// -----------------------------------------------------------------------
// fakeHeadAlwaysZero: violates "Head returns the highest seq the anchor
// knows about" (core.Anchor doc comment). Accepts and stores every
// Publish correctly, but Head always reports seq 0 -- the shape a stub
// that forgot to wire its read path to its write path would take.
// -----------------------------------------------------------------------

type fakeHeadAlwaysZero struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (f *fakeHeadAlwaysZero) Publish(_ context.Context, seq int64, head []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != 0 && seq == f.seq {
		if !bytes.Equal(f.head, head) {
			return errors.New("fakeHeadAlwaysZero: mismatched replay")
		}
		return nil
	}
	f.seq, f.head = seq, append([]byte(nil), head...)
	return nil
}

func (f *fakeHeadAlwaysZero) Head(_ context.Context) (int64, []byte, error) {
	return 0, nil, nil // BUG: never reflects what Publish recorded
}

func TestCheck_CatchesHeadAlwaysZero(t *testing.T) {
	shared := &fakeHeadAlwaysZero{}
	violations := anchortest.Check(func() core.Anchor { return shared })
	mustFindViolation(t, violations, "PublishThenHeadReflectsIt")
}

// -----------------------------------------------------------------------
// fakeHeadErrorsOnEmpty: violates "...or 0 if empty" (core.Anchor doc
// comment) -- an empty anchor must return (0, nil, nil), not an error.
// Design doc §8.4 treats an unreachable anchor as NOT_RUN, a real failure
// condition distinct from "reachable, and genuinely has nothing published
// yet"; this fake conflates the two.
// -----------------------------------------------------------------------

type fakeHeadErrorsOnEmpty struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (f *fakeHeadErrorsOnEmpty) Publish(_ context.Context, seq int64, head []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != 0 && seq == f.seq {
		if !bytes.Equal(f.head, head) {
			return errors.New("fakeHeadErrorsOnEmpty: mismatched replay")
		}
		return nil
	}
	f.seq, f.head = seq, append([]byte(nil), head...)
	return nil
}

func (f *fakeHeadErrorsOnEmpty) Head(_ context.Context) (int64, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq == 0 {
		return 0, nil, errors.New("fakeHeadErrorsOnEmpty: nothing published yet") // BUG
	}
	return f.seq, f.head, nil
}

func TestCheck_CatchesHeadErrorsOnEmpty(t *testing.T) {
	shared := &fakeHeadErrorsOnEmpty{}
	violations := anchortest.Check(func() core.Anchor { return shared })
	mustFindViolation(t, violations, "HeadOnEmptyReturnsZeroNoError")
}

// -----------------------------------------------------------------------
// fakeInMemoryOnly: violates design doc §8.3 point 3 ("可独立读取 --
// 验证方不经过账本自己的服务就能拿到 head"). Publish/Head are each
// individually correct against a single instance -- every other phase in
// the suite would pass against it -- but its factory hands back a brand
// new, empty instance every call instead of reconnecting to a shared
// backing store. This is what a "pretend anchor" that never actually left
// process memory looks like from the outside.
// -----------------------------------------------------------------------

type fakeInMemoryOnly struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (f *fakeInMemoryOnly) Publish(_ context.Context, seq int64, head []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != 0 && seq == f.seq {
		if !bytes.Equal(f.head, head) {
			return errors.New("fakeInMemoryOnly: mismatched replay")
		}
		return nil
	}
	f.seq, f.head = seq, append([]byte(nil), head...)
	return nil
}

func (f *fakeInMemoryOnly) Head(_ context.Context) (int64, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq, f.head, nil
}

func TestCheck_CatchesInMemoryOnlyFake(t *testing.T) {
	// BUG: a fresh, unconnected instance every call -- nothing shares
	// state the way two clients of a real external carrier would.
	violations := anchortest.Check(func() core.Anchor { return &fakeInMemoryOnly{} })
	mustFindViolation(t, violations, "IndependentlyConstructedClientSeesSameState")
}

// -----------------------------------------------------------------------
// correctSharedAnchor: a minimal, deliberately correct reference
// implementation -- proves the suite can also PASS, i.e. it is not just
// unconditionally red. Same shape as the four fakes above, minus the bug,
// with state shared across factory calls the way a real external carrier
// (and anchordev.LocalFileAnchor, exercised separately in anchordev's own
// tests) would be.
// -----------------------------------------------------------------------

type correctSharedAnchor struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (a *correctSharedAnchor) Publish(_ context.Context, seq int64, head []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seq != 0 && seq == a.seq {
		if !bytes.Equal(a.head, head) {
			return errors.New("correctSharedAnchor: mismatched replay")
		}
		return nil
	}
	// core.Anchor's Head contract ("MUST NEVER REGRESS"): an older seq is
	// refused rather than allowed to become the new head. This fake used to
	// overwrite unconditionally, and the suite's own
	// HeadNeverRegressesOnAnOlderPublish phase caught it the moment that
	// phase existed -- a small demonstration that the phase is not
	// vacuous.
	if seq < a.seq {
		return errors.New("correctSharedAnchor: refusing a seq older than the current head")
	}
	a.seq, a.head = seq, append([]byte(nil), head...)
	return nil
}

func (a *correctSharedAnchor) Head(_ context.Context) (int64, []byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seq, a.head, nil
}

// -----------------------------------------------------------------------
// fakeLetsHeadRegress: violates core.Anchor's "Head MUST NEVER REGRESS".
// It is the shape of this library's own R2 adapter before the 2026-09-02
// audit -- Head read a single mutable location's CURRENT value, so writing
// an older seq moved the answer backwards, which made an erased or
// rolled-back anchor indistinguishable from one that is merely behind
// (tamper-evident.md M-3/M-4).
// -----------------------------------------------------------------------

type fakeLetsHeadRegress struct {
	mu   sync.Mutex
	seq  int64
	head []byte
}

func (f *fakeLetsHeadRegress) Publish(_ context.Context, seq int64, head []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != 0 && seq == f.seq && !bytes.Equal(f.head, head) {
		return errors.New("fakeLetsHeadRegress: mismatched replay")
	}
	// BUG: whatever was written last becomes the head, older seq included.
	f.seq, f.head = seq, append([]byte(nil), head...)
	return nil
}

func (f *fakeLetsHeadRegress) Head(_ context.Context) (int64, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seq, f.head, nil
}

func TestCheck_CatchesHeadRegression(t *testing.T) {
	shared := &fakeLetsHeadRegress{}
	violations := anchortest.Check(func() core.Anchor { return shared })
	mustFindViolation(t, violations, "HeadNeverRegressesOnAnOlderPublish")
}

// TestCheck_OutOfBandPhaseIsSkippedNotPassedWithoutTheHook pins the
// working-agreements.md §3 half of the new phase: an implementation that
// does not lend the suite an out-of-band writer must see the phase reported
// as SKIPPED, never counted as a pass. A conformance suite that silently
// drops a phase it cannot run is how "we check immutability" became true on
// paper and false in fact.
func TestCheck_OutOfBandPhaseIsSkippedNotPassedWithoutTheHook(t *testing.T) {
	shared := &correctSharedAnchor{}
	skipped := anchortest.Skipped(func() core.Anchor { return shared })
	found := false
	for _, name := range skipped {
		if name == "HeadNeverRegressesAfterAnOutOfBandOlderWrite" {
			found = true
		}
	}
	if !found {
		t.Fatalf("without WithOutOfBandWrite the out-of-band phase must report as skipped; skipped = %v", skipped)
	}
}

// TestCheck_CatchesOutOfBandHeadRegression proves the out-of-band phase is
// not vacuous when the hook IS supplied: a carrier whose stored head can be
// rewritten behind Publish's back is caught.
func TestCheck_CatchesOutOfBandHeadRegression(t *testing.T) {
	shared := &fakeLetsHeadRegress{}
	violations := anchortest.Check(
		func() core.Anchor { return shared },
		anchortest.WithOutOfBandWrite(func(seq int64, head []byte) error {
			shared.mu.Lock()
			defer shared.mu.Unlock()
			shared.seq, shared.head = seq, append([]byte(nil), head...)
			return nil
		}),
	)
	mustFindViolation(t, violations, "HeadNeverRegressesAfterAnOutOfBandOlderWrite")
}

func TestCheck_PassesWellBehavedImplementation(t *testing.T) {
	shared := &correctSharedAnchor{}
	if violations := anchortest.Check(func() core.Anchor { return shared }); len(violations) != 0 {
		t.Fatalf("Check reported violations against a deliberately correct Anchor: %v", violations)
	}
}

// TestRunConformance_PassesWellBehavedImplementation exercises the actual
// exported entry point (RunConformance, not Check) end to end. It is safe
// to nest under t.Run here specifically because this case is expected to
// genuinely pass -- there is no propagation hazard when nothing fails.
func TestRunConformance_PassesWellBehavedImplementation(t *testing.T) {
	shared := &correctSharedAnchor{}
	anchortest.RunConformance(t, func() core.Anchor { return shared })
}
