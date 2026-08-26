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
