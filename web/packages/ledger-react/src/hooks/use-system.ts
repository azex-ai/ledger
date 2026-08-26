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

export function useSnapshots(params: {
  holder?: number;
  currency_uid?: string;
  start?: string;
  end?: string;
}) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.snapshots(params),
    queryFn: () => client.listSnapshots(params),
    // Negative holders (system accounts) are valid; only 0/undefined disables.
    enabled: params.holder !== undefined && params.holder !== 0,
  });
}
