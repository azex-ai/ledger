package core

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClosePeriodInput_Validate_RequiresCloseBefore(t *testing.T) {
	input := ClosePeriodInput{}
	err := input.Validate()
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrInvalidInput)
	assert.Contains(t, err.Error(), "close_before")
}

func TestClosePeriodInput_Validate_OK(t *testing.T) {
	input := ClosePeriodInput{
		CloseBefore: time.Now(),
		Note:        "month-end close",
		ActorID:     1,
	}
	require.NoError(t, input.Validate())
}
