package core

import "fmt"

// ValidateReversalFraction checks that num/den is a well-formed partial-
// reversal fraction: den must be positive, and num must fall in (0, den] —
// i.e. the reversal must cover more than 0% and at most 100% of the original
// journal. Callers (ReverseJournalFraction implementations) run this before
// touching the database so a malformed fraction never reaches a row lock.
func ValidateReversalFraction(num, den int64) error {
	if den <= 0 {
		return fmt.Errorf("core: reverse journal fraction: den must be positive, got %d: %w", den, ErrInvalidInput)
	}
	if num <= 0 || num > den {
		return fmt.Errorf("core: reverse journal fraction: num must satisfy 0 < num <= den (den=%d), got %d: %w", den, num, ErrInvalidInput)
	}
	return nil
}
