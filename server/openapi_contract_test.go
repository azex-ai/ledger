// Package server: openapi_contract_test.go
//
// This is the CI gate structure.md's Major finding says does not exist:
// "the openapi CI gate only verifies TS = spec, never spec = Go behavior".
// ledger-react.yml's codegen:check compares docs/openapi.yaml against a
// generated TS file -- both ends are static files in the same repo, neither
// one ever touches a running server or a Go handler's actual struct. This
// file closes that gap from the other end: it parses docs/openapi.yaml
// directly and cross-checks its schemas' JSON property names against the
// wire structs the handlers in this package actually decode/encode, via
// reflection on their `json` tags.
//
// This is not path-filtered and needs no server or database: it runs in
// `go test ./...`, which ci.yml's `test` job already runs unconditionally on
// every push and PR (server/*_test.go is not excluded by any path filter,
// unlike ledger-react.yml). A Go-only PR that silently drifts the wire
// format, or a docs-only PR that "fixes" openapi.yaml into a still-wrong
// shape, both fail here.
//
// Two directions, deliberately different:
//   - requestBody schemas: bidirectional (exact key-set equality). Each one
//     maps 1:1 to a single purpose-built Go struct, so both "spec documents
//     a field Go silently drops" (the expires_in bug this suite exists to
//     catch) and "Go reads an undocumented field" are real drift.
//   - response schemas: one-directional (every documented field must exist
//     on the Go struct). Several response structs (journalResponse is the
//     concrete example: shared by JournalEnvelope, JournalListEnvelope, and
//     JournalWithEntriesEnvelope) carry an extra `omitempty` field that only
//     ever serializes for ONE of several envelopes reusing the same Go type
//     -- flagging that as "undocumented" on every envelope would be a false
//     positive the reflection-only approach cannot resolve statically.
//     Catching "spec promises a field Go can never produce" is the
//     higher-value direction anyway.
package server

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

// --- docs/openapi.yaml plumbing ---

func loadOpenAPISchemas(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../docs/openapi.yaml")
	require.NoError(t, err, "read docs/openapi.yaml")
	var root map[string]any
	require.NoError(t, yaml.Unmarshal(data, &root), "parse docs/openapi.yaml")
	components, _ := root["components"].(map[string]any)
	require.NotNil(t, components, "openapi.yaml: components section missing")
	schemas, _ := components["schemas"].(map[string]any)
	require.NotNil(t, schemas, "openapi.yaml: components.schemas section missing")
	return schemas
}

func refName(ref string) string {
	const prefix = "#/components/schemas/"
	return strings.TrimPrefix(ref, prefix)
}

// resolve follows a single $ref hop. This spec never nests $ref more than
// one level inside a schema this suite reads (verified by inspection), so
// one hop is sufficient -- a second unresolved $ref will show up as a nil
// properties map and fail loudly rather than silently under-checking.
func resolve(t *testing.T, schemas map[string]any, node map[string]any) map[string]any {
	t.Helper()
	if ref, ok := node["$ref"].(string); ok {
		name := refName(ref)
		target, ok := schemas[name].(map[string]any)
		require.True(t, ok, "openapi.yaml: $ref %q does not resolve to a components.schemas entry", ref)
		return target
	}
	return node
}

// flattenAllOf merges an allOf member list's "properties" maps into one --
// every envelope schema in this spec is `allOf: [Envelope, {properties:
// {data: ...}}]`.
func flattenAllOf(t *testing.T, schemas map[string]any, schema map[string]any) map[string]any {
	t.Helper()
	allOf, ok := schema["allOf"].([]any)
	if !ok {
		return schema
	}
	mergedProps := map[string]any{}
	for _, partAny := range allOf {
		part, ok := partAny.(map[string]any)
		require.True(t, ok, "openapi.yaml: allOf member is not a mapping")
		part = resolve(t, schemas, part)
		if props, ok := part["properties"].(map[string]any); ok {
			for k, v := range props {
				mergedProps[k] = v
			}
		}
	}
	return map[string]any{"properties": mergedProps}
}

func namedSchema(t *testing.T, schemas map[string]any, name string) map[string]any {
	t.Helper()
	schema, ok := schemas[name].(map[string]any)
	require.True(t, ok, "openapi.yaml: components.schemas.%s not found", name)
	return schema
}

// flatPropertyNames returns the top-level JSON property name set a named
// (non-envelope) component schema declares.
func flatPropertyNames(t *testing.T, schemas map[string]any, name string) map[string]bool {
	t.Helper()
	schema := flattenAllOf(t, schemas, namedSchema(t, schemas, name))
	props, _ := schema["properties"].(map[string]any)
	return propKeys(props)
}

// envelopeDataObjectNames returns the property name set of
// <envelopeName>.data, resolving a $ref on data itself if present -- for
// single-object envelopes (data: Foo), not list envelopes.
func envelopeDataObjectNames(t *testing.T, schemas map[string]any, envelopeName string) map[string]bool {
	t.Helper()
	dataNode := envelopeDataNode(t, schemas, envelopeName)
	dataNode = resolve(t, schemas, dataNode)
	props, _ := dataNode["properties"].(map[string]any)
	return propKeys(props)
}

// envelopeListItemNames returns the property name set of the item schema
// under <envelopeName>.data.list.items -- for list envelopes.
func envelopeListItemNames(t *testing.T, schemas map[string]any, envelopeName string) map[string]bool {
	t.Helper()
	dataNode := envelopeDataNode(t, schemas, envelopeName)
	props, _ := dataNode["properties"].(map[string]any)
	require.NotNil(t, props, "%s: data has no properties (expected list/next_cursor)", envelopeName)
	listNode, ok := props["list"].(map[string]any)
	require.True(t, ok, "%s: data has no list property", envelopeName)
	items, ok := listNode["items"].(map[string]any)
	require.True(t, ok, "%s: list has no items schema", envelopeName)
	items = resolve(t, schemas, items)
	itemProps, _ := items["properties"].(map[string]any)
	return propKeys(itemProps)
}

func envelopeDataNode(t *testing.T, schemas map[string]any, envelopeName string) map[string]any {
	t.Helper()
	flat := flattenAllOf(t, schemas, namedSchema(t, schemas, envelopeName))
	props, _ := flat["properties"].(map[string]any)
	require.NotNil(t, props, "%s: envelope has no properties", envelopeName)
	dataNode, ok := props["data"].(map[string]any)
	require.True(t, ok, "%s: envelope has no data property", envelopeName)
	return dataNode
}

func propKeys(props map[string]any) map[string]bool {
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// --- Go struct plumbing ---

// goJSONFieldNames returns the set of top-level JSON field names a struct's
// `json` tags declare, skipping "-".
func goJSONFieldNames(t *testing.T, v any) map[string]bool {
	t.Helper()
	typ := reflect.TypeOf(v)
	require.Equal(t, reflect.Struct, typ.Kind(), "%v is not a struct", typ)
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = true
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- assertions ---

// assertExactKeys is for requestBody schemas: bidirectional drift is real
// drift (see file doc comment).
func assertExactKeys(t *testing.T, label string, specKeys, goKeys map[string]bool) {
	t.Helper()
	var missingInGo, missingInSpec []string
	for k := range specKeys {
		if !goKeys[k] {
			missingInGo = append(missingInGo, k)
		}
	}
	for k := range goKeys {
		if !specKeys[k] {
			missingInSpec = append(missingInSpec, k)
		}
	}
	sort.Strings(missingInGo)
	sort.Strings(missingInSpec)
	if len(missingInGo) > 0 || len(missingInSpec) > 0 {
		t.Errorf("%s: docs/openapi.yaml and the Go wire struct disagree on field names.\n  documented in openapi.yaml but the Go struct never reads them: %v\n  read by the Go struct but not documented in openapi.yaml: %v",
			label, missingInGo, missingInSpec)
	}
}

// assertSpecSubsetOfGo is for response schemas: every documented field must
// actually exist on the Go struct that produces it (see file doc comment
// for why the reverse direction is not checked here).
func assertSpecSubsetOfGo(t *testing.T, label string, specKeys, goKeys map[string]bool) {
	t.Helper()
	var missingInGo []string
	for k := range specKeys {
		if !goKeys[k] {
			missingInGo = append(missingInGo, k)
		}
	}
	sort.Strings(missingInGo)
	if len(missingInGo) > 0 {
		t.Errorf("%s: docs/openapi.yaml documents field(s) the Go response struct does not have: %v (Go struct fields: %v)",
			label, missingInGo, sortedKeys(goKeys))
	}
}

// --- registry: requestBody schemas <-> Go request structs ---

func TestOpenAPIContract_RequestBodiesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	cases := []struct {
		schema string
		goVal  any
	}{
		// This is the verification sample structure.md's finding calls out
		// by name: ReserveInput documented expires_in (a duration string)
		// while the Go struct reads expires_in_sec (an integer). Reverting
		// docs/openapi.yaml's fix for that field must fail this test --
		// that is the acceptance bar for this whole file.
		{"ReserveInput", createReservationRequest{}},
		{"JournalInput", postJournalRequest{}},
		{"TemplateExecutionRequest", postTemplateRequest{}},
		{"TemplatePreviewRequest", previewTemplateRequest{}},
		{"AccountPolicyInput", setAccountPolicyRequest{}},
		{"CreateBookingInput", createBookingRequest{}},
		{"TransitionInput", transitionRequest{}},
		{"ClassificationInput", createClassificationRequest{}},
		{"TemplateInput", createTemplateRequest{}},
		{"DepositToleranceRequest", postDepositToleranceRequest{}},
	}
	for _, tc := range cases {
		t.Run(tc.schema, func(t *testing.T) {
			specKeys := flatPropertyNames(t, schemas, tc.schema)
			goKeys := goJSONFieldNames(t, tc.goVal)
			assertExactKeys(t, "requestBody "+tc.schema, specKeys, goKeys)
		})
	}
}

// --- registry: single-object response envelopes <-> Go response structs ---

func TestOpenAPIContract_ResponseEnvelopesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	cases := []struct {
		envelope string
		goVal    any
	}{
		{"ReservationEnvelope", reservationResponse{}},
		{"JournalEnvelope", journalResponse{}},
		{"BookingEnvelope", bookingResponse{}},
		{"EventEnvelope", eventResponse{}},
		{"AccountPolicyEnvelope", accountPolicyResponse{}},
		{"DepositToleranceEnvelope", depositToleranceResponse{}},
	}
	for _, tc := range cases {
		t.Run(tc.envelope, func(t *testing.T) {
			specKeys := envelopeDataObjectNames(t, schemas, tc.envelope)
			goKeys := goJSONFieldNames(t, tc.goVal)
			assertSpecSubsetOfGo(t, "response "+tc.envelope, specKeys, goKeys)
		})
	}
}

// --- registry: list envelopes' item schema <-> Go per-item response struct ---

func TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	cases := []struct {
		envelope string
		goVal    any
	}{
		{"JournalListEnvelope", journalResponse{}},
		{"ReservationListEnvelope", reservationResponse{}},
		{"BookingListEnvelope", bookingResponse{}},
		{"EventListEnvelope", eventResponse{}},
		{"EntryListEnvelope", entryResponse{}},
		{"DepositReviewListEnvelope", bookingResponse{}},
		{"HolderTransactionListEnvelope", holderTransactionResponse{}},
		{"HolderBalanceListEnvelope", holderBalanceResponse{}},
		{"HolderHoldListEnvelope", holderHoldResponse{}},
	}
	for _, tc := range cases {
		t.Run(tc.envelope, func(t *testing.T) {
			specKeys := envelopeListItemNames(t, schemas, tc.envelope)
			goKeys := goJSONFieldNames(t, tc.goVal)
			assertSpecSubsetOfGo(t, "list item "+tc.envelope, specKeys, goKeys)
		})
	}
}

// --- systemic check: every next_cursor property must be nullable ---

// TestOpenAPIContract_NextCursorIsNullable is the spec-side half of
// structure.md's other Major ("next_cursor is never actually JSON null"):
// api-contract.md §6 requires next_cursor to be an explicit null when
// exhausted, not an omitted key or an empty string. This walks every
// component schema and asserts that any property literally named
// "next_cursor" is typed to allow "null" -- catching the exact class of bug
// this suite exists to prevent from coming back (a future list envelope
// added with `next_cursor: { type: string }`, no null, reproduces the
// original defect this test would not otherwise be scoped to notice).
// The Go-side half (PagedResponse actually serializes nil to literal null)
// is pinned in response_test.go.
func TestOpenAPIContract_NextCursorIsNullable(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for name, schemaAny := range schemas {
		schema, ok := schemaAny.(map[string]any)
		if !ok {
			continue
		}
		flat := flattenAllOf(t, schemas, schema)
		dataNode, _ := flat["properties"].(map[string]any)
		var props map[string]any
		if dataNode != nil {
			if data, ok := dataNode["data"].(map[string]any); ok {
				props, _ = data["properties"].(map[string]any)
			} else {
				props = dataNode
			}
		}
		nc, ok := props["next_cursor"].(map[string]any)
		if !ok {
			continue
		}
		types := nextCursorTypes(nc)
		if !containsNull(types) {
			t.Errorf("schema %s: next_cursor is typed %v, which cannot express JSON null -- api-contract.md §6 requires next_cursor:null when exhausted (add \"null\" to its type)", name, types)
		}
	}
}

func nextCursorTypes(nc map[string]any) []string {
	switch v := nc["type"].(type) {
	case string:
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func containsNull(types []string) bool {
	for _, t := range types {
		if t == "null" {
			return true
		}
	}
	return false
}
