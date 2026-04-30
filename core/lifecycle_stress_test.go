package core

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A multi-terminal lifecycle (e.g. deposit → confirmed | rejected | expired)
// must validate, and IsTerminal must return true for every listed terminal.
func TestLifecycle_MultipleTerminals(t *testing.T) {
	lc := &Lifecycle{
		Initial:  "pending",
		Terminal: []Status{"confirmed", "rejected", "expired"},
		Transitions: map[Status][]Status{
			"pending":    {"reviewing", "rejected", "expired"},
			"reviewing":  {"confirmed", "rejected"},
		},
	}
	require.NoError(t, lc.Validate())
	assert.True(t, lc.IsTerminal("confirmed"))
	assert.True(t, lc.IsTerminal("rejected"))
	assert.True(t, lc.IsTerminal("expired"))
	assert.False(t, lc.IsTerminal("reviewing"))
	assert.False(t, lc.IsTerminal("pending"))
}

// A self-loop on a non-terminal status is allowed (e.g. retry from "failed").
// This pins the design choice: the validator only forbids terminal->X edges,
// not X->X edges. Documenting via test prevents accidental tightening.
func TestLifecycle_SelfLoopAllowed(t *testing.T) {
	lc := &Lifecycle{
		Initial:  "pending",
		Terminal: []Status{"done"},
		Transitions: map[Status][]Status{
			"pending": {"failed", "done"},
			"failed":  {"failed", "done"}, // retry self-loop
		},
	}
	require.NoError(t, lc.Validate())
	assert.True(t, lc.CanTransition("failed", "failed"))
	assert.True(t, lc.CanTransition("failed", "done"))
}

// A transition that points to a status with no further transitions and which
// isn't listed as Terminal must be rejected. This is the "dead-end" guard:
// every reachable status must be either terminal-by-declaration or have a
// way out.
func TestLifecycle_DeadEndStatusRejected(t *testing.T) {
	lc := &Lifecycle{
		Initial:  "pending",
		Terminal: []Status{"done"},
		Transitions: map[Status][]Status{
			"pending": {"orphan"}, // orphan has no entry in Transitions and isn't Terminal
		},
	}
	err := lc.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
	assert.Contains(t, err.Error(), "targets undefined status")
}

// Empty terminal list with non-terminal-only transitions must still validate
// IF every reachable target has outgoing edges. This is unusual but legal —
// e.g. a polling loop that never "ends".
func TestLifecycle_NoTerminals(t *testing.T) {
	lc := &Lifecycle{
		Initial:  "polling",
		Terminal: nil,
		Transitions: map[Status][]Status{
			"polling": {"polling"}, // truly cyclic; no terminals
		},
	}
	require.NoError(t, lc.Validate())
	assert.False(t, lc.IsTerminal("polling"))
}

// A long linear chain (10 hops) — exercise that Validate scales linearly and
// CanTransition correctly walks one step at a time.
func TestLifecycle_LongChain(t *testing.T) {
	transitions := map[Status][]Status{}
	for i := range 9 {
		from := Status(rune('a' + i))
		to := Status(rune('a' + i + 1))
		transitions[from] = []Status{to}
	}
	lc := &Lifecycle{
		Initial:     "a",
		Terminal:    []Status{"j"},
		Transitions: transitions,
	}
	require.NoError(t, lc.Validate())

	// Each adjacent pair must transition; non-adjacent must not.
	for i := range 9 {
		from := Status(rune('a' + i))
		to := Status(rune('a' + i + 1))
		assert.True(t, lc.CanTransition(from, to), "%s -> %s should be allowed", from, to)
	}
	assert.False(t, lc.CanTransition("a", "c"))
	assert.False(t, lc.CanTransition("a", "j"))
}

// Initial cannot equal a terminal status (initial must have outgoing
// transitions; terminal must not). Pin this implication.
func TestLifecycle_InitialCannotBeTerminal(t *testing.T) {
	lc := &Lifecycle{
		Initial:  "done",
		Terminal: []Status{"done"},
		Transitions: map[Status][]Status{
			"done": {"done"},
		},
	}
	err := lc.Validate()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidInput))
}

// FuzzLifecycleValidate explores arbitrary lifecycle shapes. Validate must
// never panic, and on success Initial must have outgoing transitions and no
// Terminal status may have outgoing edges.
//
// Run with: go test ./core -run=^$ -fuzz=FuzzLifecycleValidate -fuzztime=10s
func FuzzLifecycleValidate(f *testing.F) {
	f.Add("a", "a,b", "a:b;b:a")
	f.Add("pending", "done", "pending:done")
	f.Add("", "done", "pending:done")
	f.Fuzz(func(t *testing.T, initial, terminalCSV, transitionsSpec string) {
		lc := parseLifecycleSpec(initial, terminalCSV, transitionsSpec)
		err := lc.Validate()
		if err == nil {
			// Property 1: initial must have outgoing transitions.
			outs, ok := lc.Transitions[lc.Initial]
			if !ok || len(outs) == 0 {
				t.Fatalf("validated lifecycle has no outgoing edge from initial %q", lc.Initial)
			}
			// Property 2: no terminal status may have outgoing edges.
			for _, term := range lc.Terminal {
				if edges, ok := lc.Transitions[term]; ok && len(edges) > 0 {
					t.Fatalf("validated lifecycle has outgoing edges from terminal %q", term)
				}
			}
		}
	})
}

// parseLifecycleSpec turns the fuzz input strings into a Lifecycle.
// Format: terminalCSV = "a,b,c"; transitionsSpec = "a:b,c;b:c"
func parseLifecycleSpec(initial, terminalCSV, transitionsSpec string) *Lifecycle {
	lc := &Lifecycle{Initial: Status(initial)}

	if terminalCSV != "" {
		start := 0
		for i := 0; i <= len(terminalCSV); i++ {
			if i == len(terminalCSV) || terminalCSV[i] == ',' {
				if i > start {
					lc.Terminal = append(lc.Terminal, Status(terminalCSV[start:i]))
				}
				start = i + 1
			}
		}
	}

	if transitionsSpec == "" {
		return lc
	}
	lc.Transitions = map[Status][]Status{}
	from := ""
	values := []Status{}
	curr := ""
	flush := func() {
		if from != "" {
			lc.Transitions[Status(from)] = append([]Status{}, values...)
		}
		from = ""
		values = values[:0]
	}
	for i := range transitionsSpec {
		c := transitionsSpec[i]
		switch c {
		case ';':
			if curr != "" {
				values = append(values, Status(curr))
				curr = ""
			}
			flush()
		case ':':
			from = curr
			curr = ""
		case ',':
			if curr != "" {
				values = append(values, Status(curr))
				curr = ""
			}
		default:
			curr += string(c)
		}
	}
	if curr != "" {
		values = append(values, Status(curr))
	}
	flush()
	return lc
}
