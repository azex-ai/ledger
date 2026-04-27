// Package delivery delivers ledger Events to external consumers. Two
// concrete deliverers ship today:
//
//   - CallbackDeliverer (callback.go) -- in-process function invocation,
//     for embedded library users who want events fanned out to local
//     code.
//   - WebhookDeliverer (webhook.go) -- HTTP POST with per-attempt
//     exponential backoff, dead-lettering after MaxAttempts, used by the
//     standalone service.
//
// Both implement core.EventDeliverer. The worker loop polls for
// undelivered events and invokes Deliver in batches; a successful
// delivery flips the persisted attempt counter and marks the row done.
package delivery
