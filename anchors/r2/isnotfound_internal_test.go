package r2

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// TestIsNotFound_CoversBothServerShapes pins the branch that only executes
// against real R2. AWS S3 returns a typed NoSuchKey for a missing GetObject;
// R2 and some other S3-compatible servers return a bare 404 instead. MinIO --
// what the conformance test runs against -- returns the typed error, so the
// 404 fallback never fires there and would otherwise ship unexercised.
//
// isNotFound is a pure function, so "we have no R2 credentials" is not a
// reason to leave it untested: a synthetic smithy response error reaches the
// same branch a real R2 404 would.
func TestIsNotFound_CoversBothServerShapes(t *testing.T) {
	resp := func(code int) error {
		return &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: code}},
			Err:      errors.New("synthetic"),
		}
	}

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"AWS S3 typed NoSuchKey", &types.NoSuchKey{}, true},
		{"wrapped typed NoSuchKey", fmt.Errorf("get: %w", &types.NoSuchKey{}), true},
		{"R2 bare 404", resp(http.StatusNotFound), true},
		{"wrapped R2 bare 404", fmt.Errorf("get: %w", resp(http.StatusNotFound)), true},
		{"403 must not read as absent", resp(http.StatusForbidden), false},
		{"500 must not read as absent", resp(http.StatusInternalServerError), false},
		{"unrelated error", errors.New("dial tcp: refused"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isNotFound(tc.err); got != tc.want {
				t.Errorf("isNotFound = %v, want %v", got, tc.want)
			}
		})
	}
}
