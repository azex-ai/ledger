import { ApiRequestError } from "../client/client";

/**
 * Surface the server's user-facing error text (already desensitized at the
 * handler boundary per api-contract.md §1) instead of a generic fallback.
 *
 * client.ts preserves `message.text` on every non-200 envelope; without this
 * the 55 `onError` toasts all collapsed to "Failed to …", hiding actionable
 * detail like idempotency-key payload mismatches or unbalanced journals.
 */
export function errorText(err: unknown, fallback: string): string {
  return err instanceof ApiRequestError && err.apiError.message
    ? err.apiError.message
    : fallback;
}

/**
 * Field-level validation errors from the server's `message.fields`
 * (api-contract.md §1: key = request-body snake_case field name, value =
 * user-facing text). Returns `{}` for anything else — never throws, so
 * callers can spread the result straight into a `setFieldErrors` call with
 * no type guard of their own.
 *
 * J-8 (2026-09-02 web audit): `err.apiError.fields` was already threaded
 * all the way from the wire (`client.ts`'s `ApiRequestError`) but no page
 * read it — every mutation's field-level errors collapsed into the same
 * generic toast as an unrelated 500. This codebase has no react-hook-form
 * (all forms are plain `useState`), so there is no `setError(field, ...)`
 * to call; the pattern here is a sibling `useState<Record<string,string>>`
 * cleared per-field on the next edit, populated from this helper in the
 * mutation's `onError`. See ClassificationsPage's CreateDialog for the
 * reference wiring.
 */
export function apiFieldErrors(err: unknown): Record<string, string> {
  return err instanceof ApiRequestError ? (err.apiError.fields ?? {}) : {};
}
