package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEntryType_IsValid(t *testing.T) {
	assert.True(t, EntryTypeDebit.IsValid())
	assert.True(t, EntryTypeCredit.IsValid())
	assert.False(t, EntryType("invalid").IsValid())
}

func TestNormalSide_IsValid(t *testing.T) {
	assert.True(t, NormalSideDebit.IsValid())
	assert.True(t, NormalSideCredit.IsValid())
	assert.False(t, NormalSide("invalid").IsValid())
}

func TestSystemAccountHolder(t *testing.T) {
	assert.Equal(t, int64(-42), SystemAccountHolder(42))
	assert.True(t, IsSystemAccount(-42))
	assert.False(t, IsSystemAccount(42))
}
