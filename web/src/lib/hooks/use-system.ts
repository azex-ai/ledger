import { useQuery, useMutation } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useHealth() {
  return useQuery({
    queryKey: ["health"],
    queryFn: api.getHealth,
    refetchInterval: 10_000,
  });
}

export function useSystemBalances() {
  return useQuery({
    queryKey: ["system-balances"],
    queryFn: api.getSystemBalances,
  });
}

export function useReconcileGlobal() {
  return useMutation({
    mutationFn: api.reconcileGlobal,
  });
}

export function useReconcileAccount() {
  return useMutation({
    mutationFn: ({ holder, currencyId }: { holder: number; currencyId: number }) =>
      api.reconcileAccount(holder, currencyId),
  });
}

export function useSnapshots(params: {
  holder?: number;
  currency_id?: number;
  start?: string;
  end?: string;
}) {
  return useQuery({
    queryKey: ["snapshots", params],
    queryFn: () => api.listSnapshots(params),
    enabled: !!params.holder,
  });
}
