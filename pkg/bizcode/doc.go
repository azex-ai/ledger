// Package bizcode is the canonical business-error registry. Every
// error surfaced over HTTP carries a numeric Code; the HTTP status is
// derived from the code's range (10000-10099 -> 400, 14000-14999 -> 422,
// etc., see AppError.HTTPStatus).
//
// AppError satisfies the standard error interface and supports
// errors.Is / errors.As, so handlers can wrap a core sentinel and
// callers further up the stack can still match on the bizcode.
//
// DisplayMessage maps a code to a stable user-facing string. The wire
// "message" field always uses the registered display message, never the
// internal reason -- this keeps internals out of API responses.
//
// # Segment allocation
//
//   - 10000-10399, 10900-10999: generic HTTP-shaped errors (input / auth /
//     forbidden / not-found / already-exists / conflict), not retryable.
//   - 10400-10499: rate limiting, retryable.
//   - 14000-14999: ledger domain-invariant violations, HTTP 422, never
//     retryable (replaying the identical request reproduces the identical
//     rejection). 14001-14009 predate this doc; 14010+ was allocated
//     2026-08-26 during the audit remediation wave (see
//     docs/plans/2026-08-26-audit-remediation-contracts.md §2 -- pkg/bizcode
//     holds sole allocation rights over this segment) starting with
//     UnauthorizedJournal (core.ErrUnauthorizedJournal), the tamper-detection
//     system's rejection signal.
//   - 18100-18199: HTTP 503, retryable -- either a genuinely transient
//     condition (RollupPending, AttestorUnavailable, TransientFailure, all
//     added 2026-08-26 to mirror core.IsRetryable, see
//     pkg/httpx/response_test.go TestResolveError_AgreesWithCoreIsRetryable)
//     or a permanent-but-looks-transient-to-the-caller state (FeatureNotEnabled).
//   - everything else (default, including 19999 Internal): unclassified,
//     retryable by default -- an error that cannot be matched to a known
//     core sentinel is assumed to be a transient dependency hiccup rather
//     than a permanent defect.
package bizcode
