package anchordev_test

import (
	"path/filepath"
	"testing"

	"github.com/azex-ai/ledger/anchordev"
	"github.com/azex-ai/ledger/anchortest"
	"github.com/azex-ai/ledger/core"
)

// TestLocalFileAnchor_Conformance is the one-line integration anchortest's
// package doc describes: a consumer's Anchor implementation (here,
// LocalFileAnchor standing in for that consumer, since this library also
// consumes its own dev implementation) proves it satisfies core.Anchor's
// contract by running anchortest.RunConformance against it.
//
// The factory below returns a brand new *LocalFileAnchor on every call but
// always pointed at the same file path -- exactly the "fresh client, same
// backing store" shape anchortest.RunConformance's doc comment requires,
// and it is not a formality here: it is what lets
// IndependentlyConstructedClientSeesSameState actually exercise
// LocalFileAnchor's on-disk persistence (a second *LocalFileAnchor value
// re-reading the file written by the first) instead of trivially reusing
// one Go value's in-memory state.
func TestLocalFileAnchor_Conformance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "anchor.txt")
	anchortest.RunConformance(t, func() core.Anchor {
		return anchordev.NewLocalFileAnchorForDevelopment(path)
	})
}
