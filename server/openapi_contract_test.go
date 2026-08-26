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

	"github.com/azex-ai/ledger/core"
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
// <envelopeName>.data, resolving a $ref on data itself if present, and
// flattening an allOf on data itself if present (e.g.
// JournalWithEntriesEnvelope's data is `allOf: [Journal, {entries: [...]}]`,
// reusing Journal's own schema instead of re-listing its fields) -- for
// single-object envelopes (data: Foo), not list envelopes.
func envelopeDataObjectNames(t *testing.T, schemas map[string]any, envelopeName string) map[string]bool {
	t.Helper()
	dataNode := envelopeDataNode(t, schemas, envelopeName)
	dataNode = resolve(t, schemas, dataNode)
	dataNode = flattenAllOf(t, schemas, dataNode)
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

// requestBodySchemaCases is the single source of truth for "which
// components.schemas entries are requestBody schemas, and which Go struct
// each one must match" -- both TestOpenAPIContract_RequestBodiesMatchGoStructs
// (field-level check) and TestOpenAPIContract_EveryRequestBodySchemaIsRegistered
// (completeness check: is every requestBody schema docs/openapi.yaml's paths
// actually reference present in this list at all) read this same slice, so
// registering a schema in only one of the two checks is not possible.
var requestBodySchemaCases = []struct {
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

func TestOpenAPIContract_RequestBodiesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for _, tc := range requestBodySchemaCases {
		t.Run(tc.schema, func(t *testing.T) {
			specKeys := flatPropertyNames(t, schemas, tc.schema)
			goKeys := goJSONFieldNames(t, tc.goVal)
			assertExactKeys(t, "requestBody "+tc.schema, specKeys, goKeys)
		})
	}
}

// --- registry: single-object response envelopes <-> Go response structs ---

// responseEnvelopeCases is the completeness source of truth for
// single-object envelopes, same rationale as requestBodySchemaCases above.
var responseEnvelopeCases = []struct {
	envelope string
	goVal    any
}{
	{"ReservationEnvelope", reservationResponse{}},
	{"JournalEnvelope", journalResponse{}},
	{"BookingEnvelope", bookingResponse{}},
	{"EventEnvelope", eventResponse{}},
	{"AccountPolicyEnvelope", accountPolicyResponse{}},
	{"DepositToleranceEnvelope", depositToleranceResponse{}},
	{"JournalWithEntriesEnvelope", journalResponse{}},
	{"BookingTraceEnvelope", bookingTraceResponse{}},
	{"BalanceBreakdownEnvelope", balanceBreakdownResponse{}},
	{"BalanceByCurrencyEnvelope", balanceByCurrencyResponse{}},
	{"HolderTokenEnvelope", mintHolderTokenResponse{}},
	{"DepositAddressEnvelope", depositAddressResponse{}},
	{"PlatformBalanceEnvelope", platformBalanceResponse{}},
	{"SolvencyEnvelope", solvencyResponse{}},
	{"ReconcileEnvelope", reconcileResponse{}},
	{"ReconcileReportEnvelope", core.ReconcileReport{}},
}

func TestOpenAPIContract_ResponseEnvelopesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for _, tc := range responseEnvelopeCases {
		t.Run(tc.envelope, func(t *testing.T) {
			specKeys := envelopeDataObjectNames(t, schemas, tc.envelope)
			goKeys := goJSONFieldNames(t, tc.goVal)
			assertSpecSubsetOfGo(t, "response "+tc.envelope, specKeys, goKeys)
		})
	}
}

// --- registry: list envelopes' item schema <-> Go per-item response struct ---

// listEnvelopeCases is the completeness source of truth for list envelopes,
// same rationale as requestBodySchemaCases above. Not every entry is named
// "*ListEnvelope" -- BalancesEnvelope and SystemRollupsEnvelope are
// list-shaped (`data: {list, next_cursor}`) under a name that doesn't say
// so; this list is keyed on shape, not name.
var listEnvelopeCases = []struct {
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
	{"BalancesEnvelope", balanceResponse{}},
	{"SystemRollupsEnvelope", systemRollupResponse{}},
	{"BalanceTrendListEnvelope", balanceTrendPointResponse{}},
}

func TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for _, tc := range listEnvelopeCases {
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

// --- completeness: every requestBody / response envelope schema paths
// actually references must be registered above, not just the ones someone
// remembered to add (M-8, 2026-08-26 independent review, second pass) ---
//
// Every check above (TestOpenAPIContract_RequestBodiesMatchGoStructs,
// TestOpenAPIContract_ResponseEnvelopesMatchGoStructs,
// TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs) only checks the
// schemas someone thought to add to *SchemaCases/*EnvelopeCases/
// *Cases above -- a new endpoint with a new named schema drifting from its
// Go struct passes silently because nothing ever looks at it. This mirrors
// postgres/grant_coverage_test.go's "a new table defaults to nothing, not
// to full access" pattern: enumerate every schema `paths` actually
// references (from the live spec, not a second hand-maintained list) and
// Fatal on anything not present in one of the three registries above.

// loadOpenAPIPaths re-parses docs/openapi.yaml and returns its top-level
// `paths` map. Deliberately a second file read rather than sharing state
// with loadOpenAPISchemas -- this file's existing helpers already accept
// that cost for simplicity, and paths/schemas are read together only in
// the two completeness tests below.
func loadOpenAPIPaths(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../docs/openapi.yaml")
	require.NoError(t, err, "read docs/openapi.yaml")
	var root map[string]any
	require.NoError(t, yaml.Unmarshal(data, &root), "parse docs/openapi.yaml")
	paths, _ := root["paths"].(map[string]any)
	require.NotNil(t, paths, "openapi.yaml: paths section missing")
	return paths
}

// schemaRefIn returns the schema name a `{$ref: "#/components/schemas/X"}`
// node names, or "" if node is nil or has no $ref (an inline schema with no
// component name -- e.g. POST /balances/batch's requestBody, or
// POST /journal-types' requestBody -- has nothing for a by-name registry to
// check against, so it is correctly invisible to both this completeness
// check and the field-level checks above; that gap is a different, larger
// finding than M-8, not silently expanded to cover here).
func schemaRefIn(node map[string]any) string {
	if node == nil {
		return ""
	}
	ref, _ := node["$ref"].(string)
	if ref == "" {
		return ""
	}
	return refName(ref)
}

// everyRequestBodySchemaRef walks every operation in paths and returns the
// set of component schema names referenced by a requestBody.
func everyRequestBodySchemaRef(t *testing.T, paths map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, methodsAny := range paths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for _, opAny := range methods {
			op, ok := opAny.(map[string]any)
			if !ok {
				continue
			}
			rb, ok := op["requestBody"].(map[string]any)
			if !ok {
				continue
			}
			content, _ := rb["content"].(map[string]any)
			appJSON, _ := content["application/json"].(map[string]any)
			schema, _ := appJSON["schema"].(map[string]any)
			if name := schemaRefIn(schema); name != "" {
				out[name] = true
			}
		}
	}
	return out
}

// everySuccessResponseSchemaRef walks every operation in paths and returns
// the set of component schema names referenced by a 2xx response. Error
// responses (403, 503, the shared "#/components/responses/DomainError"
// $ref) are deliberately out of scope -- ErrorResponse is a single generic
// shape shared across every handler's error path, not a per-endpoint
// success shape the registries above model one Go struct per schema for;
// filtering to 2xx excludes it structurally instead of by name, so a future
// success schema misnamed to look like an error schema would still be
// caught.
func everySuccessResponseSchemaRef(t *testing.T, paths map[string]any) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, methodsAny := range paths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for _, opAny := range methods {
			op, ok := opAny.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := op["responses"].(map[string]any)
			if !ok {
				continue
			}
			for code, respAny := range responses {
				if len(code) == 0 || code[0] != '2' {
					continue
				}
				resp, ok := respAny.(map[string]any)
				if !ok {
					continue // e.g. a $ref to #/components/responses/DomainError
				}
				content, _ := resp["content"].(map[string]any)
				appJSON, _ := content["application/json"].(map[string]any)
				schema, _ := appJSON["schema"].(map[string]any)
				if name := schemaRefIn(schema); name != "" {
					out[name] = true
				}
			}
		}
	}
	return out
}

// TestOpenAPIContract_EveryRequestBodySchemaIsRegistered is M-8's own pin:
// every requestBody schema docs/openapi.yaml's paths reference must appear
// in requestBodySchemaCases above. Verified red before this test existed by
// construction (see file doc comment): pick any requestBodySchemaCases
// entry, delete it, and this test fails identifying exactly that gap,
// whereas TestOpenAPIContract_RequestBodiesMatchGoStructs simply stops
// running that schema's subtest -- silent, not red.
func TestOpenAPIContract_EveryRequestBodySchemaIsRegistered(t *testing.T) {
	paths := loadOpenAPIPaths(t)
	referenced := everyRequestBodySchemaRef(t, paths)

	registered := make(map[string]bool, len(requestBodySchemaCases))
	for _, tc := range requestBodySchemaCases {
		registered[tc.schema] = true
	}

	var missing []string
	for name := range referenced {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("requestBody schema(s) %v are referenced by docs/openapi.yaml's paths but not registered in requestBodySchemaCases -- "+
			"a new requestBody schema defaults to unchecked, not to verified; add it to requestBodySchemaCases with its matching Go request struct", missing)
	}
}

// TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered is
// TestOpenAPIContract_EveryRequestBodySchemaIsRegistered's response-side
// counterpart. A schema referenced by a 2xx response must appear in either
// responseEnvelopeCases (single-object) or listEnvelopeCases (list) -- not
// necessarily both, but at least one; being in both would mean two
// contradictory shape checks run against it, which the underlying
// envelopeDataObjectNames/envelopeListItemNames helpers would fail loudly
// on for an actual list-shaped or object-shaped schema, so this test does
// not additionally guard against double-registration.
func TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered(t *testing.T) {
	paths := loadOpenAPIPaths(t)
	referenced := everySuccessResponseSchemaRef(t, paths)

	registered := make(map[string]bool, len(responseEnvelopeCases)+len(listEnvelopeCases))
	for _, tc := range responseEnvelopeCases {
		registered[tc.envelope] = true
	}
	for _, tc := range listEnvelopeCases {
		registered[tc.envelope] = true
	}

	var missing []string
	for name := range referenced {
		if !registered[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("response schema(s) %v are referenced by docs/openapi.yaml's paths under a 2xx status but not registered in responseEnvelopeCases or listEnvelopeCases -- "+
			"a new response schema defaults to unchecked, not to verified; add it to whichever of the two matches its shape, with its matching Go response struct", missing)
	}
}
