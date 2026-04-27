// Package service contains background orchestration that the ledger
// runs alongside the HTTP API: async checkpoint rollups, daily balance
// snapshots, accounting-equation reconciliation, booking-expiry sweeps,
// and the worker loop that ticks them on a schedule.
//
// Nothing here is request-scoped -- every entry point takes a context
// from a long-running goroutine and exits cleanly when that context is
// cancelled. Mutations into core stores still go through the same
// interfaces the HTTP handlers use; service code never bypasses the
// domain layer.
//
// Subpackage service/delivery handles outbound event delivery (in-process
// callbacks for library mode, HTTP webhooks for service mode).
package service
