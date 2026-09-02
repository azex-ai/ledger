package server

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// api_doc_examples_test.go holds docs/api.md's JSON examples to the wire
// rules the code actually implements.
//
// Why a gate and not a proofread: two of the three `next_cursor` examples in
// that file said `""` while every list endpoint in the server emits a literal
// `null` (server/response.go's PagedResponse makes the field a *string for
// exactly that reason, and api-contract.md §6 requires null as the
// type-stable "no next page" sentinel). A reader implementing against the
// documented example would have compared against the wrong sentinel -- the
// same class of defect as H-M1's parameter names, in prose instead of in the
// spec, and the reason the audit's territory H found drift everywhere the
// gate did not reach.
//
// The examples are the part of the documentation a consumer copies first, so
// they are contract whether or not anyone calls them that.

var jsonFencePattern = regexp.MustCompile("(?s)```json\\s*\\n(.*?)```")

// apiDocJSONExamples returns every ```json fenced block in docs/api.md that
// parses as a JSON object, paired with a 1-based fence index for reporting.
//
// A fence that does not parse is skipped rather than failed: a few blocks in
// that file are deliberately elided with `"...": "..."` placeholders or
// fragments. It fails closed on the count instead -- see the caller.
func apiDocJSONExamples(t *testing.T) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile("../docs/api.md")
	require.NoError(t, err, "docs/api.md must be readable: a gate that cannot find its subject must not pass")

	matches := jsonFencePattern.FindAllStringSubmatch(string(raw), -1)
	require.NotEmpty(t, matches, "found no ```json fences in docs/api.md")

	var out []map[string]any
	for _, m := range matches {
		var obj map[string]any
		if err := json.Unmarshal([]byte(m[1]), &obj); err != nil {
			continue
		}
		out = append(out, obj)
	}
	return out
}

// TestAPIDocExamples_NextCursorIsNullNotEmptyString pins the sentinel.
//
// `""` and `null` are two spellings of "no next page", and a consumer's
// generic paging helper can only check one. The server emits null; the docs
// must too.
func TestAPIDocExamples_NextCursorIsNullNotEmptyString(t *testing.T) {
	examples := apiDocJSONExamples(t)
	require.GreaterOrEqual(t, len(examples), 35,
		"only %d parseable JSON examples found in docs/api.md; the extractor has stopped seeing them and must not read as a pass", len(examples))

	seen := 0
	for i, obj := range examples {
		walkJSON(obj, func(path string, key string, value any) {
			if key != "next_cursor" {
				return
			}
			seen++
			switch v := value.(type) {
			case nil:
				// The documented sentinel.
			case string:
				if v == "" {
					t.Errorf("docs/api.md JSON example #%d, %s: next_cursor is \"\"; the wire value is a literal null "+
						"(server/response.go PagedResponse.NextCursor is a *string, api-contract.md §6). An empty string is a "+
						"third spelling of \"exhausted\" that no endpoint emits.", i+1, path)
				}
			default:
				t.Errorf("docs/api.md JSON example #%d, %s: next_cursor is %T; it is either a cursor string or null.", i+1, path, value)
			}
		})
	}
	require.Positive(t, seen, "no next_cursor key found in any docs/api.md example; the walk is not reaching them")
}

// TestAPIDocExamples_TimestampsAreUTC is the same idea for the `_at` fields.
//
// pkg/httpx installs a jsoniter extension whose whole purpose is that a
// deployment running in a non-UTC timezone still emits `Z` (H-M4 found the
// outbound webhook exit bypassing it). Documented examples showing a local
// offset would tell a consumer to parse something the API never sends.
func TestAPIDocExamples_TimestampsAreUTC(t *testing.T) {
	seen := 0
	for i, obj := range apiDocJSONExamples(t) {
		walkJSON(obj, func(path string, key string, value any) {
			if !strings.HasSuffix(key, "_at") {
				return
			}
			s, ok := value.(string)
			if !ok || s == "" {
				return // absent/elided values are covered by the schema gates
			}
			// Skip the fields that are documented as opaque or non-timestamp
			// (e.g. an "expires_at": null is the H-M2 wire shape).
			if !strings.Contains(s, "T") {
				return
			}
			seen++
			if !strings.HasSuffix(s, "Z") {
				t.Errorf("docs/api.md JSON example #%d, %s: %q is not UTC. Every _at field goes out as RFC3339 with a "+
					"Z suffix (api-contract.md §5; pkg/httpx's utcTimeExtension enforces it regardless of the host's TZ).",
					i+1, path, s)
			}
		})
	}
	require.Positive(t, seen, "no _at timestamp found in any docs/api.md example; the walk is not reaching them")
}

// walkJSON visits every key/value pair in a decoded JSON document, including
// inside nested objects and arrays, reporting a dotted path for messages.
func walkJSON(node any, visit func(path, key string, value any)) {
	walkJSONAt("", node, visit)
}

func walkJSONAt(path string, node any, visit func(path, key string, value any)) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			child := key
			if path != "" {
				child = path + "." + key
			}
			visit(child, key, value)
			walkJSONAt(child, value, visit)
		}
	case []any:
		for i, item := range v {
			walkJSONAt(fmt.Sprintf("%s[%d]", path, i), item, visit)
		}
	}
}
