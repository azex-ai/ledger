package core

import (
	"fmt"
	"time"
)

// PeriodClose is one append-only row in the period-close audit log. The
// active close line at any moment is the row with the latest CreatedAt
// (latest-row-wins) — see docs/plans/2026-07-02-financial-core-hardening-design.md
// §2. Reopening a period is done by appending a new row with an earlier
// CloseBefore; rows are never updated or deleted.
type PeriodClose struct {
	ID          int64     `json:"id"`
	CloseBefore time.Time `json:"close_before"`
	Note        string    `json:"note"`
	ActorID     int64     `json:"actor_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// ClosePeriodInput is the input to append a new period-close line.
type ClosePeriodInput struct {
	// CloseBefore is the new close line: journals with EffectiveAt before this
	// timestamp are rejected with ErrPeriodClosed.
	CloseBefore time.Time `json:"close_before"`
	Note        string    `json:"note"`
	ActorID     int64     `json:"actor_id"`
}

// Validate checks that the input is well-formed.
func (c *ClosePeriodInput) Validate() error {
	if c.CloseBefore.IsZero() {
		return fmt.Errorf("core: period close: close_before is required: %w", ErrInvalidInput)
	}
	return nil
}
