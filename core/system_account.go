// Package core — system_account.go
//
// System-Account Convention
// =========================
//
// The ledger uses a signed int64 "account holder" dimension to distinguish
// user-side accounts from their platform-side mirrors:
//
//	holder > 0  →  user-side account
//	holder < 0  →  system-side mirror account
//
// The mapping is a simple negation, defined in types.go:
//
//	SystemAccountHolder(userID) = -userID
//	IsSystemAccount(holder) = holder < 0
//
// This convention is intentionally opaque at the library level. Each consuming
// service decides what "userID" means for its domain:
//
//   - A multi-user payments platform might map userID → postgres user row ID.
//   - A workspace/team product might map userID → workspace row ID.
//   - A project-tracking service might map userID → project ID.
//   - A multi-tenant SaaS platform might map userID → tenant ID.
//
// The library only enforces the sign invariant. Domain-specific semantics live
// in the consuming service, not here.
//
// Usage example — deposit template (user receives, platform custodial is credited):
//
//	userHolder   := int64(42)                        // positive: user-side
//	sysHolder    := core.SystemAccountHolder(42)     // -42: system-side
//
//	// User's main wallet receives funds (debit for a debit-normal account)
//	entry1 := core.EntryInput{AccountHolder: userHolder, ..., EntryType: core.EntryTypeDebit}
//	// Platform custodial account is credited (tracks custody on the system side)
//	entry2 := core.EntryInput{AccountHolder: sysHolder, ..., EntryType: core.EntryTypeCredit}
//
// To reverse the mapping:
//
//	userHolder = core.UserHolderFromSystem(sysHolder)  // UserHolderFromSystem(-42) = 42
//
// Why NOT derive a separate "workspace ID" or "tenant ID" transform (e.g.
// payments' DeriveSystemWorkspaceId)? Because that design leaks platform-
// specific ID-space logic into a shared library. Here the rule is trivially
// reversible: UserHolder(sysHolder) == -sysHolder, no external lookup needed.
package core

// IsUserAccount reports whether the holder ID belongs to the user side.
// Returns true when holder > 0.
// holder == 0 is not assigned by convention (reserved / invalid).
func IsUserAccount(holder int64) bool {
	return holder > 0
}

// UserHolderFromSystem reverses SystemAccountHolder, returning the positive
// user-side holder for a system account. The caller must ensure holder < 0;
// passing a positive user holder will return a negative value.
//
// Example:
//
//	core.UserHolderFromSystem(-42) // returns 42
func UserHolderFromSystem(holder int64) int64 {
	return -holder
}
