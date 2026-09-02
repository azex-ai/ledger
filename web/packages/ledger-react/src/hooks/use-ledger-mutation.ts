import {
  useMutation,
  useQueryClient,
} from "@tanstack/react-query";
import { useRef } from "react";
import { ledgerKeyPrefix } from "./keys";

/**
 * A mutation function that also receives a per-attempt-sequence idempotency
 * key (see the useLedgerMutation doc comment below). Callers that don't need
 * it — the majority, whose client method has no meaningful retry-collision
 * risk or that already manage their own key (postJournal, settlePartial —
 * api-contract.md §9) — simply declare fewer parameters and ignore it; JS/TS
 * allow that (a function is assignable to a type expecting more params than
 * it declares).
 */
type LedgerMutationFn<TData, TVariables> = (
  variables: TVariables,
  idempotencyKey: string,
) => Promise<TData>;

/**
 * Wrapper around useMutation that (a) automatically invalidates
 * balance-related queries on success, and (b) hands the mutation function a
 * STABLE idempotency key across retries of the same attempt sequence (M4,
 * 2026-08-26 web audit).
 *
 * api-contract.md §9: "幂等 key 由操作发起方生成一次，跨重试复用；禁止在重试
 * 路径内重新生成" — the key belongs to the caller's *intent*, not to the HTTP
 * attempt. Minting a fresh key per manual retry (the previous behavior, via
 * `client.ts`'s per-attempt `crypto.randomUUID()` fallback) defeats the
 * ledger's own replay-receipt short-circuit: a mutation that actually
 * succeeded server-side but timed out client-side gets retried under a NEW
 * key, misses the receipt, hits the resulting state-transition guard, and
 * reports failure for an operation that already happened.
 *
 * The key is scoped per PAYLOAD, not per hook instance (J-13, 2026-09-02 web
 * audit): a single `useRef<string | null>` keyed the whole hook instance to
 * one in-flight key, which is only safe when every call site happens to be
 * a per-row component (`{ id }`) — a hook instance shared across multiple
 * entities (e.g. a page-scoped approve/reject action) would, after a failed
 * attempt on entity A, hand that SAME key to a later attempt on entity B;
 * the server's three-state idempotency semantics (same key + different
 * payload -> `ErrConflict`, `CLAUDE.md`) then permanently fail every
 * subsequent entity, because the stale key never gets cleared for a payload
 * it doesn't belong to (only a retry of A would clear it).
 *
 * `keyOf` derives a payload identity (defaults to `JSON.stringify`); a
 * `Map<payloadKey, uuid>` replaces the single ref so each payload's key is
 * independent — a failure on A no longer poisons a later attempt on B, and
 * only A's own entry is deleted on A's success. This generalizes the
 * pattern already used by hand at the call site for settle-partial
 * (ReservationsPage's `usePayloadIdempotencyKey`) into the shared wrapper so
 * every mutation gets it automatically, with zero call-site changes
 * required for the common per-row case (default `keyOf` already
 * distinguishes different variables).
 *
 *   const mutation = useLedgerMutation((body) => client.postJournal(body), ["journals"]);
 *   const mutation = useLedgerMutation((id, idempotencyKey) => client.settleX(id, idempotencyKey), ["reservations"]);
 *   const mutation = useLedgerMutation(fn, ["reviews"], (uid) => uid); // custom payload identity
 */
export function useLedgerMutation<TData, TVariables>(
  mutationFn: LedgerMutationFn<TData, TVariables>,
  invalidateKeys: string[],
  keyOf: (variables: TVariables) => string = (variables) => JSON.stringify(variables),
) {
  const qc = useQueryClient();
  const keysRef = useRef(new Map<string, string>());
  return useMutation({
    mutationFn: (variables: TVariables) => {
      const payloadKey = keyOf(variables);
      let idempotencyKey = keysRef.current.get(payloadKey);
      if (!idempotencyKey) {
        idempotencyKey = crypto.randomUUID();
        keysRef.current.set(payloadKey, idempotencyKey);
      }
      return mutationFn(variables, idempotencyKey);
    },
    onSuccess: (_data, variables) => {
      // Ready for the next distinct action on THIS payload — a fresh click
      // after a success must not reuse a key an already-completed operation
      // owns. Other in-flight/failed payloads' keys are untouched.
      keysRef.current.delete(keyOf(variables));
      for (const key of invalidateKeys) {
        // Namespace each caller-passed bare segment under the package root
        // prefix; no raw "ledger" literal lives here.
        qc.invalidateQueries({ queryKey: [...ledgerKeyPrefix.all, key] });
      }
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.balances });
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.systemBalances });
    },
    // onError intentionally does NOT delete the payload's key — a retried
    // .mutate() call with the SAME payload after a failure reuses it by
    // design.
  });
}
