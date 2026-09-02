// Package server: openapi_types_test.go
//
// The second half of the openapi contract gate. openapi_contract_test.go
// compares *top-level property name sets*; this file compares everything
// that comparison structurally could not see, each one a confirmed drift in
// the 2026-09-02 deep audit:
//
//   - H-M2 types and formats. `Booking.expires_at` was declared
//     `format: date-time` and `required`, while the Go field was a plain
//     `string` that a booking with no expiry serialized as `""` -- a third
//     state ("", null, a timestamp) no generated client can parse. Names
//     matched, so the old gate was green.
//   - H-m7 nesting. `envelopeDataObjectNames` reads one level of `data`
//     properties, so `ReconcileReport.checks[].complete` -- the flag that
//     keeps "did not run" from reading as "passed" -- had no contract
//     coverage in either direction.
//   - H-m11 direction. Response checks were spec-⊆-Go only, so a new Go
//     response field was invisible to every mechanism. Now bidirectional,
//     with a per-envelope allowlist for the genuinely shared `omitempty`
//     fields that motivated the one-directional compromise -- and the
//     allowlist itself is validated against the Go structs, so a stale
//     entry is red rather than a permanent hole.
//   - H-M5 required completeness. `required:` had been hand-added to 8
//     schemas; 7 were still fully optional, including `solvent`,
//     `overall_passed` and `full_coverage`.
//   - H-m2 version. `info.version` said 0.4.0 with the CHANGELOG at 0.6.0,
//     under a comment in the same file telling the reader to bump both.
//
// Everything here derives both sides from artifacts (the spec file, the Go
// types via reflection, CHANGELOG.md) rather than from a hand-kept list of
// what to check.
package server

import (
	stdjson "encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/azex-ai/ledger/core"
)

var timeType = reflect.TypeOf(time.Time{})

// sharedOmitemptyFields records the `omitempty` Go fields that may
// legitimately be absent from a spec schema, keyed by the Go type that
// declares them. A response struct reused by several schemas carries fields
// that only ever serialize under one of them -- journalResponse is shared by
// JournalEnvelope, JournalListEnvelope, JournalWithEntriesEnvelope and the
// nested `journals[]` of BookingTrace / DepositTolerancePlanResult, and only
// the with-entries form emits `entries`. That single case is the reason the
// response direction used to be switched off wholesale ("Go has, spec
// lacks" was unresolvable by static reflection). It is now a bounded list,
// and TestOpenAPIContract_SharedOmitemptyAllowlistIsAccurate keeps it
// honest: the field must exist on the named type, must carry `omitempty`,
// and must be documented by at least one registered schema -- so an entry
// that stops describing a shared field is red, not a permanent hole.
var sharedOmitemptyFields = map[string]map[string]bool{
	"server.journalResponse": {"entries": true},
}

// omitemptyExemptions returns the exemption set for a Go type, keyed the way
// reflect spells it ("server.journalResponse").
func omitemptyExemptions(typ reflect.Type) map[string]bool {
	if typ.Kind() == reflect.Ptr {
		typ = typ.Elem()
	}
	return sharedOmitemptyFields[typ.String()]
}

// --- spec node helpers ---

// specNode is a resolved schema node plus the nullability the enclosing
// declaration expressed.
type specNode struct {
	node     map[string]any
	nullable bool
}

// resolveNode follows $ref (repeatedly), flattens allOf, and unwraps the
// `oneOf: [{type: null}, X]` spelling this spec uses for "nullable $ref".
func resolveNode(t *testing.T, schemas map[string]any, node map[string]any) specNode {
	t.Helper()
	out := specNode{node: node}
	for i := 0; i < 8; i++ {
		if ref, ok := out.node["$ref"].(string); ok {
			target, ok := schemas[refName(ref)].(map[string]any)
			require.True(t, ok, "openapi.yaml: $ref %q does not resolve", ref)
			out.node = target
			continue
		}
		if members, ok := oneOfMembers(out.node); ok {
			var nonNull []map[string]any
			for _, m := range members {
				if isNullSchema(m) {
					out.nullable = true
					continue
				}
				nonNull = append(nonNull, m)
			}
			if len(nonNull) == 1 {
				out.node = nonNull[0]
				continue
			}
			// A genuine polymorphic union: nothing single-typed to check.
			out.node = map[string]any{}
			return out
		}
		if _, ok := out.node["allOf"]; ok {
			out.node = flattenAllOf(t, schemas, out.node)
			continue
		}
		break
	}
	if containsNull(specTypes(out.node)) {
		out.nullable = true
	}
	return out
}

func oneOfMembers(node map[string]any) ([]map[string]any, bool) {
	for _, key := range []string{"oneOf", "anyOf"} {
		raw, ok := node[key].([]any)
		if !ok {
			continue
		}
		out := make([]map[string]any, 0, len(raw))
		for _, m := range raw {
			mm, ok := m.(map[string]any)
			if !ok {
				return nil, false
			}
			out = append(out, mm)
		}
		return out, true
	}
	return nil, false
}

func isNullSchema(node map[string]any) bool {
	types := specTypes(node)
	return len(types) == 1 && types[0] == "null"
}

// specTypes normalizes `type: string` and `type: [string, "null"]`.
func specTypes(node map[string]any) []string {
	switch v := node["type"].(type) {
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

func primaryType(node map[string]any) string {
	for _, ty := range specTypes(node) {
		if ty != "null" {
			return ty
		}
	}
	return ""
}

func requiredSet(node map[string]any) map[string]bool {
	out := map[string]bool{}
	raw, _ := node["required"].([]any)
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out[s] = true
		}
	}
	return out
}

// --- Go struct helpers ---

type goField struct {
	typ       reflect.Type
	omitempty bool
}

// goJSONFields returns json name -> field type + omitempty for a struct
// type, which is what every type comparison below needs and
// goJSONFieldNames (name set only) structurally cannot express.
func goJSONFields(typ reflect.Type) map[string]goField {
	out := map[string]goField{}
	if typ.Kind() != reflect.Struct {
		return out
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		out[name] = goField{typ: f.Type, omitempty: strings.Contains(opts, "omitempty")}
	}
	return out
}

// --- the comparison ---

type compareOpts struct {
	// bidirectional reports Go fields the spec does not document (the
	// exemptions come from sharedOmitemptyFields, keyed on the Go type being
	// compared, so a shared struct stays exempt wherever it is nested).
	bidirectional bool
	// requestSide turns off the "a pointer field must be nullable or
	// absent-able" rule, which only makes sense for what the server emits.
	// On a request struct a pointer to a REQUIRED field is the idiom for
	// telling "omitted" apart from a legitimate zero value
	// (createCurrencyRequest.Exponent: 0 is a valid exponent, so a plain
	// int32 would silently accept a missing field as JPY).
	requestSide bool
}

// assertSchemaMatchesGoType walks a resolved spec object node and a Go type
// in parallel, descending through nested objects and arrays.
func assertSchemaMatchesGoType(t *testing.T, schemas map[string]any, path string, sn specNode, typ reflect.Type, opts compareOpts) {
	t.Helper()

	ptr := typ.Kind() == reflect.Ptr
	if ptr {
		typ = typ.Elem()
	}

	specType := primaryType(sn.node)
	props, hasProps := sn.node["properties"].(map[string]any)

	// Nullability: a spec type that admits null needs a Go representation
	// that can emit null. A pointer where the spec is not nullable is only
	// legitimate as "absent from the wire", which the caller checks via
	// omitempty + not-required at the parent level.
	if sn.nullable && !ptr && !canBeJSONNull(typ) {
		t.Errorf("%s: spec allows null but the Go type is %s, which never serializes as null", path, typ)
	}

	switch {
	case specType == "object" || (specType == "" && hasProps):
		switch typ.Kind() {
		case reflect.Struct:
			if typ == timeType {
				t.Errorf("%s: spec says object, Go type is time.Time", path)
				return
			}
			if !hasProps {
				// A free-form object (e.g. `data: {}` on the Envelope base) --
				// nothing to descend into.
				return
			}
			fields := goJSONFields(typ)
			required := requiredSet(sn.node)
			for name, propAny := range props {
				prop, ok := propAny.(map[string]any)
				if !ok {
					continue
				}
				f, ok := fields[name]
				if !ok {
					t.Errorf("%s.%s: documented in docs/openapi.yaml but %s has no such json field", path, name, typ)
					continue
				}
				child := resolveNode(t, schemas, prop)
				if !opts.requestSide && f.typ.Kind() == reflect.Ptr && !child.nullable && !(f.omitempty && !required[name]) {
					t.Errorf("%s.%s: Go type is a pointer (%s) but the spec neither allows null nor lets the field be absent (add \"null\" to its type, or drop it from `required` and give the Go field `omitempty`)", path, name, f.typ)
				}
				assertSchemaMatchesGoType(t, schemas, path+"."+name, child, f.typ,
					compareOpts{bidirectional: opts.bidirectional, requestSide: opts.requestSide})
			}
			if opts.bidirectional {
				exempt := omitemptyExemptions(typ)
				var undocumented []string
				for name := range fields {
					if _, ok := props[name]; ok || exempt[name] {
						continue
					}
					undocumented = append(undocumented, name)
				}
				sort.Strings(undocumented)
				if len(undocumented) > 0 {
					t.Errorf("%s: %s serializes field(s) %v that docs/openapi.yaml does not document -- document them, or (for a field only one of several envelopes sharing this struct emits) register them in sharedOmitemptyFields", path, typ, undocumented)
				}
			}
		case reflect.Map:
			if add, ok := sn.node["additionalProperties"].(map[string]any); ok {
				assertSchemaMatchesGoType(t, schemas, path+"[*]", resolveNode(t, schemas, add), typ.Elem(), compareOpts{})
			}
		case reflect.Interface:
			// any: nothing statically checkable.
		default:
			t.Errorf("%s: spec says object, Go type is %s", path, typ)
		}

	case specType == "array":
		if typ.Kind() != reflect.Slice && typ.Kind() != reflect.Array {
			t.Errorf("%s: spec says array, Go type is %s", path, typ)
			return
		}
		items, ok := sn.node["items"].(map[string]any)
		if !ok {
			return
		}
		assertSchemaMatchesGoType(t, schemas, path+"[]", resolveNode(t, schemas, items), typ.Elem(),
			compareOpts{bidirectional: opts.bidirectional, requestSide: opts.requestSide})

	case specType == "string":
		format, _ := sn.node["format"].(string)
		if typ == timeType {
			if format != "date-time" && format != "date" {
				t.Errorf("%s: Go type is time.Time but the spec declares format %q (expected date-time)", path, format)
			}
			return
		}
		// A type with its own MarshalJSON that emits a JSON string satisfies
		// `type: string` even though its Kind is not String -- decimal.Decimal
		// is the case that matters (financial.md: amounts cross the wire as
		// strings, and this is how they do it on the paths that hand a
		// decimal.Decimal straight to the encoder, e.g. the outbound event
		// payload). Decided by marshalling the zero value rather than by a
		// hardcoded type list, so any future wire type answers for itself.
		if typ.Kind() != reflect.String && !marshalsToJSONString(typ) {
			t.Errorf("%s: spec says string (format %q), Go type is %s and does not marshal to a JSON string", path, format, typ)
		}

	case specType == "integer":
		switch typ.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		default:
			t.Errorf("%s: spec says integer, Go type is %s", path, typ)
		}

	case specType == "number":
		t.Errorf("%s: spec says number -- monetary and precise values cross this API as strings (financial.md); no wire field should be a JSON number", path)

	case specType == "boolean":
		if typ.Kind() != reflect.Bool {
			t.Errorf("%s: spec says boolean, Go type is %s", path, typ)
		}
	}
}

// marshalsToJSONString reports whether typ's zero value serializes as a JSON
// string (i.e. it has a MarshalJSON that quotes itself).
func marshalsToJSONString(typ reflect.Type) bool {
	zero := reflect.New(typ).Elem().Interface()
	data, err := stdjson.Marshal(zero)
	return err == nil && len(data) > 0 && data[0] == '"'
}

func canBeJSONNull(typ reflect.Type) bool {
	switch typ.Kind() {
	case reflect.Ptr, reflect.Map, reflect.Slice, reflect.Interface:
		return true
	default:
		return false
	}
}

// TestOpenAPIContract_RequestBodyTypesMatchGoStructs is the requestBody half
// of H-M2: same registry as the name-level check, now comparing declared
// types, formats and nested shapes.
func TestOpenAPIContract_RequestBodyTypesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)
	for _, tc := range requestBodySchemaCases {
		t.Run(tc.schema, func(t *testing.T) {
			sn := resolveNode(t, schemas, namedSchema(t, schemas, tc.schema))
			assertSchemaMatchesGoType(t, schemas, "requestBody "+tc.schema, sn, reflect.TypeOf(tc.goVal),
				compareOpts{bidirectional: true, requestSide: true})
		})
	}
}

// TestOpenAPIContract_ResponseTypesMatchGoStructs is H-M2 + H-m7 + H-m11 on
// the response side: types, arbitrary nesting, and both directions.
func TestOpenAPIContract_ResponseTypesMatchGoStructs(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	for _, tc := range responseEnvelopeCases {
		t.Run(tc.envelope, func(t *testing.T) {
			dataNode := envelopeDataNode(t, schemas, tc.envelope)
			sn := resolveNode(t, schemas, dataNode)
			assertSchemaMatchesGoType(t, schemas, tc.envelope+".data", sn, reflect.TypeOf(tc.goVal),
				compareOpts{bidirectional: true})
		})
	}

	for _, tc := range listEnvelopeCases {
		t.Run(tc.envelope, func(t *testing.T) {
			dataNode := envelopeDataNode(t, schemas, tc.envelope)
			props, _ := dataNode["properties"].(map[string]any)
			require.NotNil(t, props, "%s: data has no properties", tc.envelope)
			listNode, ok := props["list"].(map[string]any)
			require.True(t, ok, "%s: data has no list property", tc.envelope)
			items, ok := listNode["items"].(map[string]any)
			require.True(t, ok, "%s: list has no items schema", tc.envelope)
			sn := resolveNode(t, schemas, items)
			assertSchemaMatchesGoType(t, schemas, tc.envelope+".data.list[]", sn, reflect.TypeOf(tc.goVal),
				compareOpts{bidirectional: true})
		})
	}
}

// TestOpenAPIContract_SharedOmitemptyAllowlistIsAccurate keeps the
// bidirectional check's escape hatch honest: an entry that no longer
// describes a real shared omitempty field is a hole nobody would notice.
func TestOpenAPIContract_SharedOmitemptyAllowlistIsAccurate(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	// Every registered Go type, and every json property name any registered
	// schema documents -- both derived from the registries, not listed here.
	types := map[string]reflect.Type{}
	documented := map[string]bool{}
	for _, tc := range responseEnvelopeCases {
		types[reflect.TypeOf(tc.goVal).String()] = reflect.TypeOf(tc.goVal)
		for name := range envelopeDataObjectNames(t, schemas, tc.envelope) {
			documented[name] = true
		}
	}
	for _, tc := range listEnvelopeCases {
		types[reflect.TypeOf(tc.goVal).String()] = reflect.TypeOf(tc.goVal)
		for name := range envelopeListItemNames(t, schemas, tc.envelope) {
			documented[name] = true
		}
	}

	for typeName, fields := range sharedOmitemptyFields {
		typ, ok := types[typeName]
		require.True(t, ok, "sharedOmitemptyFields names %q, which no registered response case uses -- delete the entry", typeName)
		goFields := goJSONFields(typ)
		for name := range fields {
			f, ok := goFields[name]
			require.True(t, ok, "sharedOmitemptyFields[%s] exempts %q, but the type has no such json field -- delete the entry", typeName, name)
			require.True(t, f.omitempty, "sharedOmitemptyFields[%s] exempts %q, but the Go field has no omitempty, so it is always on the wire and must be documented", typeName, name)
			require.True(t, documented[name], "sharedOmitemptyFields[%s] exempts %q, but no registered schema documents it anywhere -- the exemption is for fields ONE of several schemas emits, not for undocumented fields", typeName, name)
		}
	}
}

// TestOpenAPIContract_EveryResponseObjectDeclaresRequired is H-M5: a 2xx
// object schema with properties and no `required` generates every field as
// `T | undefined`, which for `full_coverage` / `complete` / `solvent`
// erases exactly the "did not run is not a pass" distinction those fields
// exist to carry. Derived from the artifact (walk every 2xx schema,
// including inline nesting) rather than from a list of schemas someone
// remembered.
func TestOpenAPIContract_EveryResponseObjectDeclaresRequired(t *testing.T) {
	schemas := loadOpenAPISchemas(t)
	paths := loadOpenAPIPaths(t)

	var missing []string
	seen := map[string]bool{}
	for name := range everySuccessResponseSchemaRef(t, paths) {
		walkRequiredCompleteness(t, schemas, name, namedSchema(t, schemas, name), seen, &missing)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("object schema(s) reachable from a 2xx response declare properties but no `required`: %v\n"+
			"openapi-typescript renders every property of such a schema as optional, which is a different contract than the Go handler's -- "+
			"add `required` listing the fields the handler always emits (a field is optional only when the Go struct has `omitempty` and can genuinely be absent)", missing)
	}
}

func walkRequiredCompleteness(t *testing.T, schemas map[string]any, path string, node map[string]any, seen map[string]bool, missing *[]string) {
	t.Helper()
	if node == nil || seen[path] {
		return
	}
	seen[path] = true

	sn := resolveNode(t, schemas, node)
	props, hasProps := sn.node["properties"].(map[string]any)
	if hasProps && len(props) > 0 {
		if _, ok := sn.node["required"]; !ok {
			*missing = append(*missing, path)
		}
		for name, propAny := range props {
			prop, ok := propAny.(map[string]any)
			if !ok {
				continue
			}
			// The generic envelope's own `data: {}` and `message` are shared
			// glue with their own schemas; descend anyway, `seen` keeps it
			// finite.
			walkRequiredCompleteness(t, schemas, path+"."+name, prop, seen, missing)
		}
	}
	if items, ok := sn.node["items"].(map[string]any); ok {
		walkRequiredCompleteness(t, schemas, path+"[]", items, seen, missing)
	}
	if add, ok := sn.node["additionalProperties"].(map[string]any); ok {
		walkRequiredCompleteness(t, schemas, path+"[*]", add, seen, missing)
	}
}

// TestOpenAPIContract_VersionMatchesChangelog is H-m2. docs/openapi.yaml's
// own header says "bump both together, not independently"; this makes that
// sentence executable.
func TestOpenAPIContract_VersionMatchesChangelog(t *testing.T) {
	specVersion := openAPIInfoVersion(t)

	data, err := os.ReadFile("../CHANGELOG.md")
	require.NoError(t, err, "read CHANGELOG.md")
	re := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)
	m := re.FindStringSubmatch(string(data))
	require.NotNil(t, m, "CHANGELOG.md has no `## [X.Y.Z]` heading")

	require.Equal(t, m[1], specVersion,
		"docs/openapi.yaml's info.version (%s) and CHANGELOG.md's latest release (%s) disagree -- the spec's own header requires bumping both together; a spec-generated client otherwise labels itself with the wrong API version",
		specVersion, m[1])
}

func openAPIInfoVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../docs/openapi.yaml")
	require.NoError(t, err, "read docs/openapi.yaml")
	var root map[string]any
	require.NoError(t, yaml.Unmarshal(data, &root), "parse docs/openapi.yaml")
	info, _ := root["info"].(map[string]any)
	require.NotNil(t, info, "openapi.yaml: info section missing")
	version, _ := info["version"].(string)
	require.NotEmpty(t, version, "openapi.yaml: info.version missing")
	return version
}

// TestOpenAPIContract_NoOpenAPI30OnlyKeywords is H-m6's pin. The spec
// declares `openapi: 3.1.0`, where `nullable` is not a JSON Schema keyword
// -- generators ignore it, so a `nullable: true` expresses nothing while
// reading as if it did (`settled_amount` carried one, and the Go field is
// actually absent-from-the-wire, not null). A repo-local lint rather than a
// spectral/redocly step: it needs no npm install, so it runs in the same
// `go test ./...` every other contract gate runs in, on every PR.
func TestOpenAPIContract_NoOpenAPI30OnlyKeywords(t *testing.T) {
	data, err := os.ReadFile("../docs/openapi.yaml")
	require.NoError(t, err, "read docs/openapi.yaml")

	// Keyword -> what 3.1 wants instead. Matched as YAML keys (`  key:`), so
	// the same word inside a description is not a false positive.
	banned := map[string]string{
		"nullable":         `use type: [<type>, "null"], or oneOf: [{type: "null"}, ...]`,
		"x-nullable":       `use type: [<type>, "null"]`,
		"exclusiveMinimum": "3.1 takes a number, not a boolean; write the bound directly",
		"exclusiveMaximum": "3.1 takes a number, not a boolean; write the bound directly",
	}

	var findings []string
	for i, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		key, _, found := strings.Cut(trimmed, ":")
		if !found {
			continue
		}
		key = strings.TrimPrefix(key, "- ")
		hint, banned := banned[key]
		if !banned {
			continue
		}
		if key == "exclusiveMinimum" || key == "exclusiveMaximum" {
			// Only the 3.0 boolean form is wrong; a numeric bound is valid 3.1.
			_, val, _ := strings.Cut(trimmed, ":")
			val = strings.TrimSpace(val)
			if val != "true" && val != "false" {
				continue
			}
		}
		findings = append(findings, fmt.Sprintf("docs/openapi.yaml:%d: %q -- %s", i+1, key, hint))
	}
	require.Empty(t, findings, "OpenAPI 3.0-only keyword(s) in a 3.1.0 spec: generators silently ignore them, so the declaration promises nothing while reading as if it did")
}

// TestOpenAPIContract_OutboundEventMatchesCoreEvent is H-M4's schema half.
//
// The outbound webhook payload is core.Event's own json shape, and it had no
// machine-checkable description anywhere: not in openapi.yaml, not in a JSON
// Schema, nowhere. It is one of this library's most important outbound
// contracts (a subscriber parses it on every state transition), and adding a
// json tag to core.Event silently changed it.
//
// The schema is components.schemas.OutboundEvent, deliberately separate from
// Event (which is server's eventResponse, reached over REST). This check
// lives in the server package because that is where the spec-vs-Go
// comparison machinery lives; the type it checks is core's.
func TestOpenAPIContract_OutboundEventMatchesCoreEvent(t *testing.T) {
	schemas := loadOpenAPISchemas(t)
	sn := resolveNode(t, schemas, namedSchema(t, schemas, "OutboundEvent"))
	assertSchemaMatchesGoType(t, schemas, "OutboundEvent", sn, reflect.TypeOf(core.Event{}),
		compareOpts{bidirectional: true})

	// The decimal amounts must be declared as strings, not numbers
	// (financial.md): a subscriber that parses them as JSON numbers loses
	// precision, and this payload is the only place the amounts arrive
	// without an HTTP handler having stringified them first.
	props, _ := sn.node["properties"].(map[string]any)
	for _, field := range []string{"amount", "settled_amount"} {
		prop, ok := props[field].(map[string]any)
		require.True(t, ok, "OutboundEvent has no %s property", field)
		require.Equal(t, "string", primaryType(resolveNode(t, schemas, prop).node),
			"%s must cross the wire as a string", field)
	}
}
