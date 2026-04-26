import { useQuery } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useBalances(holder: number) {
  return useQuery({
    queryKey: ["balances", holder],
    queryFn: () => api.getBalances(holder),
    enabled: holder > 0,
    refetchInterval: 15_000,
  });
}

export function useBalancesByCurrency(holder: number, currency: number) {
  return useQuery({
    queryKey: ["balances", holder, currency],
    queryFn: () => api.getBalancesByCurrency(holder, currency),
    enabled: holder > 0 && currency > 0,
    refetchInterval: 15_000,
  });
}
