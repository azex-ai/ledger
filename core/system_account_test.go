package core

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsUserAccount(t *testing.T) {
	cases := []struct {
		holder int64
		want   bool
		desc   string
	}{
		{42, true, "positive user holder"},
		{1, true, "smallest user holder"},
		{math.MaxInt64, true, "max int64"},
		{0, false, "zero is not user-side"},
		{-1, false, "negative is system-side"},
		{-42, false, "negative system mirror"},
		{math.MinInt64 + 1, false, "deep negative"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			assert.Equal(t, tc.want, IsUserAccount(tc.holder))
		})
	}
}

func TestUserHolderFromSystem(t *testing.T) {
	assert.Equal(t, int64(42), UserHolderFromSystem(-42))
	assert.Equal(t, int64(1), UserHolderFromSystem(-1))
}

// SystemAccountHolder ↔ UserHolderFromSystem must be exact inverses
// for any positive user ID. This pins the convention so a future "smart"
// transform can't silently break the round-trip.
func TestSystemAccountHolder_RoundTrip(t *testing.T) {
	cases := []int64{1, 42, 1_000_000, math.MaxInt64 - 1}
	for _, user := range cases {
		sys := SystemAccountHolder(user)
		assert.True(t, IsSystemAccount(sys), "system holder must be negative for user %d", user)
		assert.False(t, IsUserAccount(sys), "system holder must not register as user for %d", user)
		assert.Equal(t, user, UserHolderFromSystem(sys), "round trip failed for %d", user)
	}
}

func TestSystemAccountHolder_ZeroIsReserved(t *testing.T) {
	// 0 is by convention "unassigned / invalid"; SystemAccountHolder(0) == 0
	// which means IsUserAccount and IsSystemAccount both return false.
	holder := SystemAccountHolder(0)
	assert.Equal(t, int64(0), holder)
	assert.False(t, IsUserAccount(holder))
	assert.False(t, IsSystemAccount(holder))
}
