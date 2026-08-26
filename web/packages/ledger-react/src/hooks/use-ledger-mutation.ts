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
 * The key is minted lazily on the first attempt, reused across every retry
 * that follows a failure, and cleared on success — so the NEXT distinct
 * click (a new logical operation) gets a fresh identity. This is the same
 * pattern already used by hand at the call site for settle-partial
 * (ReservationsPage) generalized into the shared wrapper so every mutation
 * gets it automatically, with zero call-site changes required.
 *
 *   const mutation = useLedgerMutation((body) => client.postJournal(body), ["journals"]);
 *   const mutation = useLedgerMutation((id, idempotencyKey) => client.settleX(id, idempotencyKey), ["reservations"]);
 */
export function useLedgerMutation<TData, TVariables>(
  mutationFn: LedgerMutationFn<TData, TVariables>,
  invalidateKeys: string[],
) {
  const qc = useQueryClient();
  const idempotencyKeyRef = useRef<string | null>(null);
  return useMutation({
    mutationFn: (variables: TVariables) => {
      if (!idempotencyKeyRef.current) {
        idempotencyKeyRef.current = crypto.randomUUID();
      }
      return mutationFn(variables, idempotencyKeyRef.current);
    },
    onSuccess: () => {
      // Ready for the next distinct action — a fresh click after a success
      // must not reuse a key an already-completed operation owns.
      idempotencyKeyRef.current = null;
      for (const key of invalidateKeys) {
        // Namespace each caller-passed bare segment under the package root
        // prefix; no raw "ledger" literal lives here.
        qc.invalidateQueries({ queryKey: [...ledgerKeyPrefix.all, key] });
      }
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.balances });
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.systemBalances });
    },
    // onError intentionally does NOT clear idempotencyKeyRef — a retried
    // .mutate() call after a failure reuses the same key by design.
  });
}
