package server

import (
	"testing"
)

func TestParseAPIKeys(t *testing.T) {
	t.Run("valid triples", func(t *testing.T) {
		keys, err := parseAPIKeys("ops:admin:s3cr3t, app:write:t0k3n ,report:read:r34d")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(keys) != 3 {
			t.Fatalf("want 3 keys, got %d", len(keys))
		}
		if keys[0].Name != "ops" || keys[0].Scope != ScopeAdmin || string(keys[0].Secret) != "s3cr3t" {
			t.Errorf("keys[0] = %+v", keys[0])
		}
		if keys[1].Name != "app" || keys[1].Scope != ScopeWrite || string(keys[1].Secret) != "t0k3n" {
			t.Errorf("keys[1] = %+v", keys[1])
		}
		if keys[2].Name != "report" || keys[2].Scope != ScopeRead || string(keys[2].Secret) != "r34d" {
			t.Errorf("keys[2] = %+v", keys[2])
		}
	})

	t.Run("empty is nil", func(t *testing.T) {
		keys, err := parseAPIKeys("  ")
		if err != nil || keys != nil {
			t.Fatalf("want nil,nil; got %v,%v", keys, err)
		}
	})

	t.Run("legacy bare key rejected", func(t *testing.T) {
		if _, err := parseAPIKeys("just-a-secret"); err == nil {
			t.Fatal("want error for bare key without name:scope")
		}
	})

	t.Run("unknown scope rejected", func(t *testing.T) {
		if _, err := parseAPIKeys("ops:root:x"); err == nil {
			t.Fatal("want error for unknown scope")
		}
	})

	t.Run("duplicate name rejected", func(t *testing.T) {
		if _, err := parseAPIKeys("ops:admin:a,ops:read:b"); err == nil {
			t.Fatal("want error for duplicate name")
		}
	})

	t.Run("empty name or secret rejected", func(t *testing.T) {
		if _, err := parseAPIKeys(":admin:x"); err == nil {
			t.Fatal("want error for empty name")
		}
		if _, err := parseAPIKeys("ops:admin:"); err == nil {
			t.Fatal("want error for empty secret")
		}
	})
}

func TestScopeAllows(t *testing.T) {
	cases := []struct {
		key, required Scope
		want          bool
	}{
		{ScopeRead, ScopeRead, true},
		{ScopeRead, ScopeWrite, false},
		{ScopeRead, ScopeAdmin, false},
		{ScopeWrite, ScopeRead, true},
		{ScopeWrite, ScopeWrite, true},
		{ScopeWrite, ScopeAdmin, false},
		{ScopeAdmin, ScopeRead, true},
		{ScopeAdmin, ScopeWrite, true},
		{ScopeAdmin, ScopeAdmin, true},
	}
	for _, c := range cases {
		if got := c.key.allows(c.required); got != c.want {
			t.Errorf("%s.allows(%s) = %v, want %v", c.key, c.required, got, c.want)
		}
	}
}
