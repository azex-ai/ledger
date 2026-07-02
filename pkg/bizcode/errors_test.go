package bizcode

import "testing"

func TestRetryable(t *testing.T) {
	cases := []struct {
		name string
		code int
		want bool
	}{
		{"invalid input", 10001, false},
		{"unauthorized", 10101, false},
		{"forbidden", 10150, false},
		{"not found", 10201, false},
		{"already exists", 10301, false},
		{"rate limited", 10401, true},
		{"rate limited range end", 10499, true},
		{"state conflict", 10901, false},
		{"insufficient balance", 14001, false},
		{"duplicate journal", 14002, false},
		{"unbalanced journal", 14003, false},
		{"invalid transition", 14004, false},
		{"reservation expired", 14005, false},
		{"period closed", 14006, false},
		{"service unavailable", 18101, true},
		{"internal error", 19999, true},
		{"unclassified code", 99999, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Retryable(tc.code); got != tc.want {
				t.Errorf("Retryable(%d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
