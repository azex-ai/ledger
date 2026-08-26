package main

import (
	"regexp"
	"testing"
)

// staleCheckCountPattern matches "<number> ... check(s)", the shape of the
// operability.md Minor finding this file pins: the --full flag's usage text
// once said "run all 10 reconcile checks" while service/reconcile.go's
// suite actually ran 14 -- a hardcoded number that silently drifted out of
// sync with the check list it was describing, with nothing to catch it.
// Against the OLD text ("run all 10 reconcile checks; default is just the
// global accounting equation") this pattern matches, so this test would
// have failed before the fix. It stays useful going forward: any future
// usage string that reintroduces a bare check count reintroduces the same
// bug.
var staleCheckCountPattern = regexp.MustCompile(`\d+\s+(?:reconcile\s+)?checks?\b`)

func TestReconcileFullFlagUsage_DoesNotHardcodeACheckCount(t *testing.T) {
	if staleCheckCountPattern.MatchString(reconcileFullFlagUsage) {
		t.Fatalf("--full usage text hardcodes a check count that will silently drift out of sync with service/reconcile.go's check list, the same shape of bug this test pins: %q", reconcileFullFlagUsage)
	}
}
