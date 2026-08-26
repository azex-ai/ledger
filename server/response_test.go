package server

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPagedResponse_NextCursorSerializesToLiteralNull is the Go-side pin for
// structure.md's Major finding: next_cursor never actually became JSON
// null. Before this fix, PagedResponse.NextCursor was a bare string with
// `omitempty` -- an exhausted page either dropped the key entirely
// (undefined in JS, not comparable to null) or (in handler_holder.go's
// separate, inconsistent type) serialized "". Neither satisfies
// api-contract.md §6, which requires the literal value null as a
// type-stable, comparable sentinel.
func TestPagedResponse_NextCursorSerializesToLiteralNull(t *testing.T) {
	// Exhausted: no cursor to hand back.
	exhausted := PagedResponse[string]{List: []string{"a"}, NextCursor: cursorPtr("")}
	body, err := json.Marshal(exhausted)
	require.NoError(t, err)
	assert.JSONEq(t, `{"list":["a"],"next_cursor":null}`, string(body))

	// Has more: next_cursor carries the opaque cursor string.
	hasMore := PagedResponse[string]{List: []string{"a"}, NextCursor: cursorPtr("cursor-123")}
	body, err = json.Marshal(hasMore)
	require.NoError(t, err)
	assert.JSONEq(t, `{"list":["a"],"next_cursor":"cursor-123"}`, string(body))

	// Never assigned (the "this endpoint has no concept of pagination"
	// case, e.g. handleListClassifications): still literal null, and
	// truthfully so -- there is in fact no next page, ever.
	var unpaginated PagedResponse[string]
	unpaginated.List = []string{"a"}
	body, err = json.Marshal(unpaginated)
	require.NoError(t, err)
	assert.JSONEq(t, `{"list":["a"],"next_cursor":null}`, string(body))
}

// TestCursorPtr pins the "" (exhausted) <-> nil <-> null translation
// directly, independent of JSON marshaling.
func TestCursorPtr(t *testing.T) {
	assert.Nil(t, cursorPtr(""))
	require.NotNil(t, cursorPtr("abc"))
	assert.Equal(t, "abc", *cursorPtr("abc"))
}
