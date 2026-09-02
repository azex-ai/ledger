// Package wirejson holds this repository's single JSON encoder for anything
// that leaves the process: HTTP responses and outbound webhook payloads.
//
// It exists because the two used to be different. pkg/httpx built a private
// jsoniter API with a UTC extension whose whole purpose was "never emit
// +08:00 on an _at field" -- and service/delivery/webhook.go marshalled
// core.Event with encoding/json, so the SAME event carried
// 2026-09-02T04:00:00Z to an HTTP reader and 2026-09-02T12:00:00+08:00 to a
// webhook subscriber on a TZ=Asia/Singapore deployment (H-M4). The rule was
// implemented correctly and then not connected to the second exit.
//
// Anything that serializes for a consumer goes through here. Adding a third
// exit means importing this package, not writing a third copy of the
// extensions -- a third copy is a third thing to drift.
//
// It is internal/ deliberately: it is a shared implementation detail, not
// part of the library's public contract, so it does not put jsoniter in any
// consumer's dependency graph decisions or in the Go API surface the apidiff
// gate tracks.
package wirejson

import (
	"strings"
	"time"
	"unicode"
	"unsafe"

	jsoniter "github.com/json-iterator/go"
	"github.com/modern-go/reflect2"
)

// api is a PRIVATE jsoniter API with the snake_case naming strategy and the
// UTC time encoder scoped to this package. It must never be the global
// registry: extra.SetNamingStrategy calls jsoniter.RegisterExtension, whose
// extension list is a process-wide var shared by EVERY jsoniter Config in
// the process. Importing this package (transitively, via server) would then
// silently rename every un-tagged exported field in the CONSUMER's own
// structs to snake_case -- an invisible side effect on the host process
// (abstractions.md: a library must not mutate global state). Registering the
// extensions on this one frozen API keeps the behavior identical for our
// payloads and invisible to everyone else.
var api = func() jsoniter.API {
	// Same options as ConfigCompatibleWithStandardLibrary, but a distinct
	// frozen instance so RegisterExtension below stays local to it.
	cfg := jsoniter.Config{
		EscapeHTML:             true,
		SortMapKeys:            true,
		ValidateJsonRawMessage: true,
	}.Froze()
	cfg.RegisterExtension(&snakeCaseExtension{})
	cfg.RegisterExtension(&utcTimeExtension{})
	return cfg
}()

// API returns the shared wire encoder. Callers that need decoding or
// streaming (pkg/httpx decodes request bodies with it) use this; callers
// that only serialize should use Marshal.
func API() jsoniter.API { return api }

// Marshal serializes v for the wire: snake_case field names for un-tagged
// exported fields, and every time.Time as RFC3339 in UTC.
func Marshal(v any) ([]byte, error) { return api.Marshal(v) }

// utcTimeExtension forces every time.Time in a payload to serialize as
// RFC3339 in UTC (api-contract.md §5, working-agreements.md §6: the wire is
// always `...Z`, never a local offset). Without it the output depended on the
// process TZ: pgx v5 decodes timestamptz into time.Local, and the default
// time.Time marshaler keeps that offset -- so a deployment with
// TZ=Asia/Singapore would silently emit `+08:00` on every _at field.
// Enforcing it here, once, is structural: no handler or deliverer has to
// remember to call .UTC() on each field.
type utcTimeExtension struct {
	jsoniter.DummyExtension
}

var timeType = reflect2.TypeOf(time.Time{})

func (e *utcTimeExtension) CreateEncoder(typ reflect2.Type) jsoniter.ValEncoder {
	if typ == timeType {
		return utcTimeEncoder{}
	}
	return nil
}

type utcTimeEncoder struct{}

func (utcTimeEncoder) IsEmpty(ptr unsafe.Pointer) bool {
	return (*time.Time)(ptr).IsZero()
}

func (utcTimeEncoder) Encode(ptr unsafe.Pointer, stream *jsoniter.Stream) {
	stream.WriteString((*time.Time)(ptr).UTC().Format(time.RFC3339))
}

// snakeCaseExtension applies SnakeCase to any exported field that has no
// explicit json name -- a package-scoped reimplementation of
// extra.SetNamingStrategy's extension that we register on our private API
// instead of the global one.
type snakeCaseExtension struct {
	jsoniter.DummyExtension
}

func (e *snakeCaseExtension) UpdateStructDescriptor(desc *jsoniter.StructDescriptor) {
	for _, binding := range desc.Fields {
		name := binding.Field.Name()
		if unicode.IsLower(rune(name[0])) || name[0] == '_' {
			continue
		}
		if tag, ok := binding.Field.Tag().Lookup("json"); ok {
			first := strings.Split(tag, ",")[0]
			if first == "-" || first != "" {
				continue // hidden or explicitly named
			}
		}
		binding.ToNames = []string{SnakeCase(name)}
		binding.FromNames = []string{SnakeCase(name)}
	}
}

// SnakeCase converts Go PascalCase field names to snake_case,
// correctly handling consecutive uppercase runs like "ID", "URL", "HTTP".
// Examples: ID→id, CurrencyID→currency_id, HTTPStatus→http_status, IsActive→is_active
func SnakeCase(name string) string {
	runes := []rune(name)
	n := len(runes)
	var buf []rune
	for i := 0; i < n; i++ {
		r := runes[i]
		if r >= 'A' && r <= 'Z' {
			// Find the end of consecutive uppercase run
			j := i + 1
			for j < n && runes[j] >= 'A' && runes[j] <= 'Z' {
				j++
			}
			runLen := j - i
			if i > 0 {
				buf = append(buf, '_')
			}
			if runLen == 1 || j == n {
				// Single uppercase or uppercase run at end: "Is" → "is", "ID" → "id"
				for k := i; k < j; k++ {
					buf = append(buf, runes[k]-'A'+'a')
				}
			} else {
				// Uppercase run followed by lowercase: "HTTPStatus" → "http_status"
				for k := i; k < j-1; k++ {
					buf = append(buf, runes[k]-'A'+'a')
				}
				buf = append(buf, '_')
				buf = append(buf, runes[j-1]-'A'+'a')
			}
			i = j - 1
		} else {
			buf = append(buf, r)
		}
	}
	return string(buf)
}
