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
