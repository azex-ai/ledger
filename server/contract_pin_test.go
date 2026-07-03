package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestContract_NoInternalIDKeysInJSON pins invariant I-18: no HTTP request or
// response body may carry an internal bigint identifier. External identity is
// the uid (UUIDv7) exclusively; internal ids exist only inside storage.
//
// The pin is a mechanical source scan of every non-test file in this package:
// any struct json tag naming an internal id column is a contract violation.
func TestContract_NoInternalIDKeysInJSON(t *testing.T) {
	banned := regexp.MustCompile(`json:"(id|currency_id|classification_id|journal_type_id|booking_id|reservation_id|event_id|journal_id|reversal_of|template_id)[,"]`)

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if m := banned.FindString(line); m != "" {
				t.Errorf("%s:%d exposes internal id key %q in a JSON body (I-18)", f, i+1, m)
			}
		}
	}
}
