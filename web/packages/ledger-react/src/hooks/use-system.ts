import { useQuery, useMutation } from "@tanstack/react-query";
import { useRef } from "react";
import { useLedgerClient } from "../provider/context";
import { ledgerKeys } from "./keys";

export function useHealth() {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.health(),
    queryFn: () => client.getHealth(),
    refetchInterval: 10_000,
  });
}

export function useSystemBalances() {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.systemBalances(),
    queryFn: () => client.getSystemBalances(),
  });
}

export function useReconcileGlobal() {
  const client = useLedgerClient();
  // Stable across retries of a failed run, cleared on success so the next
  // deliberate "Run Global Check" click gets its own identity, not a replay
  // of a stale result (api-contract.md §9).
  const idempotencyKeyRef = useRef<string | null>(null);
  return useMutation({
    mutationFn: () => {
      if (!idempotencyKeyRef.current) idempotencyKeyRef.current = crypto.randomUUID();
      return client.reconcileGlobal(idempotencyKeyRef.current);
    },
    onSuccess: () => {
      idempotencyKeyRef.current = null;
    },
  });
}

export function useReconcileAccount() {
  const client = useLedgerClient();
  return useMutation({
    mutationFn: ({ holder, currencyUid }: { holder: number; currencyUid: string }) =>
      client.reconcileAccount(holder, currencyUid),
  });
}

/**
 * `GET /api/v1/snapshots` — ALL FOUR params are hard-required server-side
 * (`server/handler_system.go`'s `handleListSnapshots` 400s if any one is
 * missing): `holder`, `currency_uid`, `start`, `end`. Missing any of them
 * disables the query rather than firing a request that's guaranteed to 400.
 *
 * `isDisabled` (J-18, 2026-09-02 web audit) makes that gating explicit to
 * callers — without it, a missing param produces the exact same
 * `isLoading: false, isError: false, data: undefined` shape as "the request
 * ran and found nothing", which every gated query in this package silently
 * collapsed into before this fix.
 */
export function useSnapshots(params: {
  holder?: number;
  currency_uid?: string;
  start?: string;
  end?: string;
}) {
  const client = useLedgerClient();
  // Negative holders (system accounts) are valid; only 0/undefined disables.
  const isDisabled =
    params.holder === undefined ||
    params.holder === 0 ||
    !params.currency_uid ||
    !params.start ||
    !params.end;
  const query = useQuery({
    queryKey: ledgerKeys.snapshots(params),
    queryFn: () => client.listSnapshots(params),
    enabled: !isDisabled,
  });
  return { ...query, isDisabled };
}
