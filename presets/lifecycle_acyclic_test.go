package presets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/azex-ai/ledger/core"
)

// hasCycle reports whether lc's transition graph can revisit a status, and
// names one status on the cycle if so.
func hasCycle(lc *core.Lifecycle) (bool, core.Status) {
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	state := map[core.Status]int{}

	var walk func(core.Status) (bool, core.Status)
	walk = func(s core.Status) (bool, core.Status) {
		state[s] = onStack
		for _, next := range lc.Transitions[s] {
			switch state[next] {
			case onStack:
				return true, next
			case unvisited:
				if found, at := walk(next); found {
					return true, at
				}
			}
		}
		state[s] = done
		return false, ""
	}

	for s := range lc.Transitions {
		if state[s] == unvisited {
			if found, at := walk(s); found {
				return true, at
			}
		}
	}
	return false, ""
}

// TestDepositLifecycle_IsAcyclic_BecauseOnchainKeysOnStatusAlone is a load-
// bearing gate, not a property test for its own sake.
//
// service.depositTransitionKey derives Transition's mandatory idempotency key
// (I-3) as (booking_uid, to_status) for all five deposit call sites in
// service/onchain.go, and that is only safe because a deposit booking reaches
// any given status at most once, ever. Its doc comment says so. Nothing
// checked it.
//
// Add a retry edge to DepositLifecycle -- say failed -> confirming, which is
// an entirely plausible feature -- and the key silently starts colliding: the
// second, legitimate transition matches the first one's receipt and is
// swallowed as a replay, so the deposit never confirms while every layer
// reports success. That is the silent-failure shape this whole audit kept
// finding (working-agreements.md §3), and a prose note in a doc comment is
// not a defense against it (§5: what can be machine-checked should be).
//
// If this test fails because the lifecycle legitimately needs a cycle, the
// fix is NOT to delete the test -- it is to give those call sites a key that
// carries something distinguishing the two visits, the way the sweep saga's
// keys already do (sweepReviveKey / sweepSentKey take a tx hash or channel
// ref for exactly this reason).
func TestDepositLifecycle_IsAcyclic_BecauseOnchainKeysOnStatusAlone(t *testing.T) {
	cyclic, at := hasCycle(DepositLifecycle)
	require.False(t, cyclic,
		"DepositLifecycle can now revisit %q, which breaks service.depositTransitionKey's "+
			"(booking, to_status) idempotency key -- a legitimate second transition would be "+
			"swallowed as a replay. Give the deposit call sites a distinguishing key component "+
			"before landing this lifecycle change.", at)
}

// TestSweepLifecycle_IsCyclic_WhichIsWhyItsKeysCarryMore is the other half:
// it pins the reason the sweep call sites cannot use the same shortcut. If
// the sweep lifecycle ever loses its cycle, this test failing is the prompt
// to reconsider -- not a bug.
func TestSweepLifecycle_IsCyclic_WhichIsWhyItsKeysCarryMore(t *testing.T) {
	cyclic, _ := hasCycle(SweepLifecycle)
	assert.True(t, cyclic,
		"SweepLifecycle is expected to revisit statuses (failed -> pending -> sent -> failed); "+
			"sweepReviveKey/sweepSentKey/sweepFailedKey carry a tx hash or channel ref because "+
			"of it. If the cycle is gone, revisit whether those keys still need the extra component.")
}

// allPresetLifecycles is every lifecycle this package ships. The gate above
// named two of them by hand and WithdrawalLifecycle was in neither -- it has
// never been through hasCycle at all, despite carrying a retry edge
// (failed -> reserved) and a status the expiration sweep keys on
// (2026-09-02 audit F-m10). Enumerating them in one place is what stops the
// next lifecycle from being added outside the gate's field of view.
func allPresetLifecycles() map[string]*core.Lifecycle {
	return map[string]*core.Lifecycle{
		"DepositLifecycle":    DepositLifecycle,
		"WithdrawalLifecycle": WithdrawalLifecycle,
		"SweepLifecycle":      SweepLifecycle,
	}
}

// TestPresetLifecycles_ExpiredIsTerminal_BecauseTheSweepKeysOnBookingAlone is
// the counterpart to the deposit gate, for a different call site with a
// different key shape.
//
// service/expiration.go keys its transition "expire-booking-" + booking uid,
// with no status and nothing else. Its own comment explains the assumption
// exactly: a booking only turns up in ListExpiredBookings while its current
// status still has an outgoing "expired" edge, so once the transition lands
// the booking drops out of the query for good -- "unless some future custom
// classification defines a lifecycle where expired is itself non-terminal and
// re-reachable".
//
// That is a precise, checkable condition sitting in a comment. If "expired"
// ever gains an outgoing edge, a booking can reach it twice and the second,
// legitimate expiry is swallowed as a replay of the first: no error, no log,
// the booking simply never expires again. Same silent-failure shape as the
// deposit key (working-agreements.md §3).
//
// WithdrawalLifecycle is cyclic AND has an expired status, which is precisely
// the combination that makes the distinction matter: the cycle is confined to
// failed -> reserved -> processing -> failed, and "expired" sits outside it
// as a terminal leaf. Cyclic is fine here. Re-reachable "expired" is not.
func TestPresetLifecycles_ExpiredIsTerminal_BecauseTheSweepKeysOnBookingAlone(t *testing.T) {
	for name, lc := range allPresetLifecycles() {
		t.Run(name, func(t *testing.T) {
			outgoing, present := lc.Transitions["expired"]
			if !present {
				return // no expiry path at all; the sweep never touches it
			}
			assert.Emptyf(t, outgoing,
				"%s lets a booking leave \"expired\" (to %v), so it can arrive there twice -- but "+
					"service/expiration.go keys that transition on the booking uid alone, so the second "+
					"expiry is swallowed as a replay of the first and the booking silently never expires. "+
					"Either keep \"expired\" terminal, or give the sweep a key that distinguishes the passes.",
				name, outgoing)

			for from, tos := range lc.Transitions {
				for _, to := range tos {
					if to != "expired" {
						continue
					}
					assert.NotEqualf(t, core.Status("expired"), from,
						"%s has a self-edge into \"expired\"", name)
				}
			}
		})
	}
}

// TestWithdrawalLifecycle_IsCyclic_SoItsKeysMustCarryMore records the fact the
// gate above depends on, so that a future change flattening the retry path
// shows up as a deliberate decision rather than as silence.
func TestWithdrawalLifecycle_IsCyclic_SoItsKeysMustCarryMore(t *testing.T) {
	cyclic, at := hasCycle(WithdrawalLifecycle)
	assert.Truef(t, cyclic,
		"WithdrawalLifecycle is expected to revisit statuses via its failed -> reserved retry edge; "+
			"any caller deriving an idempotency key as (booking, to_status) for this lifecycle would "+
			"collide. If the cycle is gone, revisit that. (cycle at %q)", at)
}
