import { useRef } from "react";

/**
 * Like `usePayloadIdempotencyKey` below, but for a hook instance shared
 * across MULTIPLE concurrent entities (J-13, 2026-09-02 web audit) — e.g. a
 * page-scoped approve/reject action where the operator can act on several
 * queue rows without waiting for each to settle. A single-payload tracker
 * (`usePayloadIdempotencyKey`) would hand entity B the key an in-flight or
 * failed attempt on entity A still owns, and the server's three-state
 * idempotency semantics (same key + different payload -> `ErrConflict`,
 * `CLAUDE.md`) would then permanently fail B.
 *
 * `keyFor(payloadKey)` mints lazily and reuses across retries of that exact
 * payload; `clear(payloadKey)` removes only that entry (call on success).
 */
export function useKeyedIdempotencyKeys(): {
  keyFor: (payloadKey: string) => string;
  clear: (payloadKey: string) => void;
} {
  const keysRef = useRef(new Map<string, string>());
  return {
    keyFor: (payloadKey: string) => {
      let key = keysRef.current.get(payloadKey);
      if (!key) {
        key = crypto.randomUUID();
        keysRef.current.set(payloadKey, key);
      }
      return key;
    },
    clear: (payloadKey: string) => {
      keysRef.current.delete(payloadKey);
    },
  };
}

/**
 * One idempotency key per submitted payload, not per dialog lifetime.
 *
 * `crypto.randomUUID()` is minted once per distinct payload and reused
 * across retries of that exact submission (api-contract.md §9 — "同一次逻辑
 * 提交复用同一个 key, 禁止重试路径内重新生成"). If the payload changes (e.g.
 * an operator corrects a rejected amount before resubmitting), a fresh key
 * is minted — otherwise the ledger's own three-state idempotency semantics
 * (same key + different payload -> `ErrConflict`, see `CLAUDE.md`) turn the
 * correction into a dead end: the stale key keeps getting rejected for as
 * long as the dialog stays open (M7, 2026-08-26 web audit).
 *
 * `payload` should be a string that captures everything the request body
 * varies on (e.g. the amount for a settle-partial dialog). Call `reset()`
 * when the dialog closes so the next open starts clean.
 */
export function usePayloadIdempotencyKey(): {
  keyFor: (payload: string) => string;
  reset: () => void;
} {
  const keyRef = useRef("");
  const payloadRef = useRef<string | null>(null);

  return {
    keyFor: (payload: string) => {
      if (keyRef.current === "" || payloadRef.current !== payload) {
        keyRef.current = crypto.randomUUID();
        payloadRef.current = payload;
      }
      return keyRef.current;
    },
    reset: () => {
      keyRef.current = "";
      payloadRef.current = null;
    },
  };
}
