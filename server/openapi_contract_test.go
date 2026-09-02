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
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
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

// flattenAllOf merges an allOf member list's "properties" and "required"
// into one node -- every envelope schema in this spec is `allOf: [Envelope,
// {properties: {data: ...}}]`, so the base member is where `required:
// [code, message, data]` lives while the overlay is where `data`'s actual
// shape lives. Both halves have to survive the merge:
// openapi_types_test.go's required-completeness walk reads `required`, and
// dropping it here would report every envelope in the spec as missing one.
func flattenAllOf(t *testing.T, schemas map[string]any, schema map[string]any) map[string]any {
	t.Helper()
	allOf, ok := schema["allOf"].([]any)
	if !ok {
		return schema
	}
	mergedProps := map[string]any{}
	var mergedRequired []any
	seenRequired := map[string]bool{}
	for _, partAny := range allOf {
		part, ok := partAny.(map[string]any)
		require.True(t, ok, "openapi.yaml: allOf member is not a mapping")
		part = resolve(t, schemas, part)
		if props, ok := part["properties"].(map[string]any); ok {
			for k, v := range props {
				mergedProps[k] = v
			}
		}
		req, _ := part["required"].([]any)
		for _, r := range req {
			s, ok := r.(string)
			if !ok || seenRequired[s] {
				continue
			}
			seenRequired[s] = true
			mergedRequired = append(mergedRequired, r)
		}
	}
	out := map[string]any{"properties": mergedProps}
	if len(mergedRequired) > 0 {
		out["required"] = mergedRequired
	}
	return out
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
	{"ClosePeriodRequest", closePeriodRequest{}},
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
	{"ReconcileReportEnvelope", reconcileReportResponse{}},
	{"PeriodCloseEnvelope", periodCloseResponse{}},
	{"TrialBalanceEnvelope", trialBalanceResponse{}},
	{"HealthEnvelope", healthResponse{}},
	{"ClassificationEnvelope", classificationResponse{}},
	{"JournalTypeEnvelope", journalTypeResponse{}},
	{"TemplateEnvelope", templateResponse{}},
	{"CurrencyEnvelope", currencyResponse{}},
	{"TemplatePreviewResultEnvelope", previewTemplateResponse{}},
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
	{"PeriodCloseListEnvelope", periodCloseResponse{}},
	{"AccountPolicyListEnvelope", accountPolicyResponse{}},
	{"ClassificationListEnvelope", classificationResponse{}},
	{"JournalTypeListEnvelope", journalTypeResponse{}},
	{"TemplateListEnvelope", templateResponse{}},
	{"CurrencyListEnvelope", currencyResponse{}},
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
//
// m-5 (2026-08-26 independent review, third pass): the old version of this
// loop derived `props` by hardcoding exactly two shapes ("data.properties"
// or, failing that, the schema's own top-level "properties") and then did
// `nc, ok := props["next_cursor"]; if !ok { continue }` -- a schema whose
// next_cursor lived at any OTHER nesting was silently skipped, not flagged,
// indistinguishable from "this schema legitimately has no next_cursor"
// (working-agreements §3: "未运行 ≠ 通过"). findNextCursorProperties below
// replaces the two-shape guess with a recursive walk of the schema's own
// inline property tree -- it finds a "next_cursor" property regardless of
// how many levels of inline `type: object` it is nested under, so a future
// envelope's shape cannot silently defeat this check the way the old
// derivation could. It deliberately does not follow $ref: next_cursor is
// envelope glue, never part of a shared/$ref'd model, so refusing to cross
// a $ref both keeps this finite (no cycles, no need for a depth bound) and
// keeps it from wandering into an unrelated list-item schema that happens
// to reuse the same property name.
func TestOpenAPIContract_NextCursorIsNullable(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for name, schemaAny := range schemas {
		schema, ok := schemaAny.(map[string]any)
		if !ok {
			continue
		}
		flat := flattenAllOf(t, schemas, schema)
		for _, nc := range findNextCursorProperties(flat) {
			types := nextCursorTypes(nc)
			if !containsNull(types) {
				t.Errorf("schema %s: next_cursor is typed %v, which cannot express JSON null -- api-contract.md §6 requires next_cursor:null when exhausted (add \"null\" to its type)", name, types)
			}
		}
	}
}

// findNextCursorProperties recursively collects every property schema
// literally named "next_cursor" reachable from node's own "properties" map
// by descending through inline (non-$ref) nested objects. See the m-5 note
// on TestOpenAPIContract_NextCursorIsNullable above for why this replaces a
// two-shape hardcoded guess.
func findNextCursorProperties(node map[string]any) []map[string]any {
	props, _ := node["properties"].(map[string]any)
	var found []map[string]any
	for key, valAny := range props {
		val, ok := valAny.(map[string]any)
		if !ok {
			continue
		}
		if key == "next_cursor" {
			found = append(found, val)
			continue
		}
		if _, isRef := val["$ref"]; isRef {
			continue
		}
		found = append(found, findNextCursorProperties(val)...)
	}
	return found
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

// TestOpenAPIContract_Every2xxHasSchema is MJ-2's own completeness pin: a
// 2xx response with a prose `description` and no `content` produces nothing
// for openapi-typescript to generate a type from, so the frontend hand-copies
// the shape from Go source instead of consuming a generated type -- and a
// Go wire struct renaming a field drifts silently, because the field-level
// checks above (TestOpenAPIContract_ResponseEnvelopesMatchGoStructs /
// TestOpenAPIContract_ListEnvelopeItemsMatchGoStructs) only run against
// schemas someone already thought to register, and
// TestOpenAPIContract_EveryResponseEnvelopeSchemaIsRegistered only catches a
// missing registration for a schema that is already $ref'd -- neither one
// looks at a response with no schema at all. This walks every operation's
// 2xx responses directly (not derived from any registry) and asserts each
// carries a content.application/json.schema, $ref'd or inline -- inline is
// accepted deliberately, same as requestBody/response schemas the
// completeness gates above leave unregistered by design (e.g. POST
// /balances/batch, GET /snapshots: their Go response types are
// function-local, so there is no addressable struct a by-name registry
// could check them against; an inline schema is still real enough for
// codegen to produce a type from, which is this test's actual concern).
func TestOpenAPIContract_Every2xxHasSchema(t *testing.T) {
	paths := loadOpenAPIPaths(t)

	var missing []string
	for pathKey, methodsAny := range paths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for method, opAny := range methods {
			switch strings.ToUpper(method) {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
			default:
				continue
			}
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
					// A bare $ref to #/components/responses/X. No 2xx
					// response in this spec is shaped that way today (every
					// $ref'd shared response -- DomainError, NotFound -- is
					// used only for error statuses), so this branch is
					// unreached in practice; it exists so a future one fails
					// loudly here instead of panicking on the type assertion
					// below, rather than being silently treated as covered.
					missing = append(missing, method+" "+pathKey+" ["+code+"] (unresolved $ref response)")
					continue
				}
				content, _ := resp["content"].(map[string]any)
				appJSON, _ := content["application/json"].(map[string]any)
				if _, ok := appJSON["schema"]; !ok {
					missing = append(missing, strings.ToUpper(method)+" "+pathKey+" ["+code+"]")
				}
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("2xx response(s) %v have a prose description but no application/json schema -- "+
			"a response defaults to an undocumented shape, not to a generated type; add "+
			"content.application/json.schema ($ref an existing/new component when the Go response "+
			"struct is addressable, or an inline schema when it is not, e.g. a function-local response type)",
			missing)
	}
}

// enumerateRoutes builds a Server with just a router (handlers are registered
// as method values and never invoked, so nil deps are fine) and walks the chi
// tree, returning every "METHOD /path" the server actually serves, with the
// /api/v1 prefix stripped so it lines up with openapi.yaml's server-relative
// path keys. Probe/webhook/metrics paths that are intentionally undocumented
// are excluded.
func enumerateRoutes(t *testing.T) map[string]bool {
	t.Helper()
	s := &Server{router: chi.NewRouter()}
	s.setupRoutes()

	// Paths served but intentionally not part of the documented REST surface.
	undocumented := map[string]bool{
		"GET /system/health": true,
		"GET /system/ready":  true,
	}

	routes := map[string]bool{}
	err := chi.Walk(s.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimPrefix(route, "/api/v1")
		// chi appends a trailing slash for subrouter mounts; normalize it away.
		if len(route) > 1 {
			route = strings.TrimSuffix(route, "/")
		}
		key := method + " " + route
		if undocumented[key] {
			return nil
		}
		routes[key] = true
		return nil
	})
	require.NoError(t, err, "walk chi routes")
	return routes
}

// TestOpenAPIContract_EveryRouteIsDocumented closes the endpoint-level gap the
// spec->Go completeness tests above cannot see: those all walk openapi.yaml
// and check Go has what the spec names, so an endpoint that exists in the chi
// router but was never added to openapi.yaml (GET /periods/closes,
// GET /reports/trial-balance, POST /periods/close were all missing) is
// invisible to them. This walks the OTHER direction -- every registered route
// must have a matching method+path in the spec -- so a new handler that skips
// the spec goes red on its own PR, not years later. Same "derive from the
// artifact, not a hand-maintained list" discipline as the grant-coverage gate.
func TestOpenAPIContract_EveryRouteIsDocumented(t *testing.T) {
	specPaths := loadOpenAPIPaths(t)

	// Build the set of documented "METHOD /path" pairs from the spec.
	documented := map[string]bool{}
	for pathKey, methodsAny := range specPaths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for method := range methods {
			m := strings.ToUpper(method)
			switch m {
			case "GET", "POST", "PUT", "PATCH", "DELETE":
				documented[m+" "+pathKey] = true
			}
		}
	}

	var missing []string
	for route := range enumerateRoutes(t) {
		if !documented[route] {
			missing = append(missing, route)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("route(s) %v are served by the chi router but absent from docs/openapi.yaml -- "+
			"a new endpoint defaults to undocumented, invisible to every spec->Go check; add it to openapi.yaml (or to enumerateRoutes' undocumented set if it is deliberately not part of the REST surface)", missing)
	}
}

// --- registry: inline (unnamed) requestBody schemas <-> Go request structs ---
//
// H-M3: an operation whose requestBody schema is written inline instead of
// as a `$ref` to components.schemas was invisible to every check in this
// file -- schemaRefIn returns "" for it, so it landed in neither the
// "registered" nor the "unregistered" set but in a third state, unchecked
// and unreported. Thirteen requestBodies were in that state, five of them
// the reservation settle/release paths, and one of them
// (POST /journals/{uid}/reverse) documented `idempotency_key` as REQUIRED
// while the Go struct did not read the field at all: a caller believed it
// was scoping the replay window of a money-correcting write and was not.
//
// Inline is a legitimate choice for a body with no reusable shape, so the
// fix is to register those bodies by route (there is no schema name to key
// on) rather than to force a component for each. Completeness is enforced
// by TestOpenAPIContract_EveryRequestBodyIsRegistered below, which walks
// every requestBody in the spec and demands each one be covered by exactly
// one of the two registries -- so a new inline body defaults to red, not to
// invisible.
var inlineRequestBodyCases = []struct {
	route string // "METHOD /path", as spelled in openapi.yaml's paths keys
	goVal any
}{
	{"POST /journals/{uid}/reverse", reverseJournalRequest{}},
	{"POST /journals/{uid}/reverse-partial", reverseJournalFractionRequest{}},
	{"POST /balances/batch", batchBalancesRequest{}},
	{"POST /reservations/{uid}/settle", settleReservationRequest{}},
	{"POST /reservations/{uid}/settle-partial", settlePartialReservationRequest{}},
	{"POST /reservations/{uid}/finalize", terminalReservationOpRequest{}},
	{"POST /reservations/{uid}/release", terminalReservationOpRequest{}},
	{"POST /deposits/{uid}/review/reject", rejectDepositReviewRequest{}},
	{"POST /journal-types", createJournalTypeRequest{}},
	{"POST /currencies", createCurrencyRequest{}},
	{"POST /reconcile/account", reconcileAccountRequest{}},
	{"POST /holder-tokens", mintHolderTokenRequest{}},
	{"POST /dev/credits", devCreditRequest{}},
}

// requestBodySchemas returns every operation's application/json requestBody
// schema node, keyed "METHOD /path".
func requestBodySchemas(t *testing.T, paths map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for pathKey, methodsAny := range paths {
		methods, ok := methodsAny.(map[string]any)
		if !ok {
			continue
		}
		for method, opAny := range methods {
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
			if schema == nil {
				continue
			}
			out[strings.ToUpper(method)+" "+pathKey] = schema
		}
	}
	return out
}

func TestOpenAPIContract_InlineRequestBodiesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)
	bodies := requestBodySchemas(t, loadOpenAPIPaths(t))

	for _, tc := range inlineRequestBodyCases {
		t.Run(tc.route, func(t *testing.T) {
			schema, ok := bodies[tc.route]
			require.True(t, ok, "inlineRequestBodyCases names %s, which has no application/json requestBody in docs/openapi.yaml -- delete the entry", tc.route)
			require.Empty(t, schemaRefIn(schema), "%s's requestBody is now a $ref; move it to requestBodySchemaCases and delete the inline entry", tc.route)

			specKeys := propKeys(map[string]any(nil))
			if props, ok := schema["properties"].(map[string]any); ok {
				specKeys = propKeys(props)
			}
			assertExactKeys(t, "inline requestBody "+tc.route, specKeys, goJSONFieldNames(t, tc.goVal))

			// Types, formats and nesting, same walk the named schemas get.
			assertSchemaMatchesGoType(t, schemas, "inline requestBody "+tc.route,
				resolveNode(t, schemas, schema), reflect.TypeOf(tc.goVal),
				compareOpts{bidirectional: true, requestSide: true})
		})
	}
}

// TestOpenAPIContract_EveryRequestBodyIsRegistered is the completeness gate
// that closes H-M3's third state: every requestBody in the spec, $ref'd or
// inline, must be covered by one of the two registries.
func TestOpenAPIContract_EveryRequestBodyIsRegistered(t *testing.T) {
	bodies := requestBodySchemas(t, loadOpenAPIPaths(t))

	byName := make(map[string]bool, len(requestBodySchemaCases))
	for _, tc := range requestBodySchemaCases {
		byName[tc.schema] = true
	}
	byRoute := make(map[string]bool, len(inlineRequestBodyCases))
	for _, tc := range inlineRequestBodyCases {
		byRoute[tc.route] = true
	}

	var unchecked []string
	for route, schema := range bodies {
		if name := schemaRefIn(schema); name != "" {
			if !byName[name] {
				unchecked = append(unchecked, route+" ($ref "+name+")")
			}
			continue
		}
		if !byRoute[route] {
			unchecked = append(unchecked, route+" (inline)")
		}
	}
	sort.Strings(unchecked)
	if len(unchecked) > 0 {
		t.Fatalf("requestBody(ies) %v are declared in docs/openapi.yaml but checked against no Go struct -- "+
			"a requestBody defaults to unchecked, not to verified; register a $ref'd one in requestBodySchemaCases (by schema name) or an inline one in inlineRequestBodyCases (by route)", unchecked)
	}
}
