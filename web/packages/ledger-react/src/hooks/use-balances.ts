import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { ledgerKeys } from "./keys";

export function useBalances(holder: number) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.balances(holder),
    queryFn: () => client.getBalances(holder),
    // Holder 0 means "no account"; any non-zero holder is valid — system
    // counterparts are NEGATIVE holders, so don't gate on holder > 0.
    enabled: holder !== 0,
    refetchInterval: 15_000,
  });
}

export function useBalancesByCurrency(holder: number, currency: string) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.balancesByCurrency(holder, currency),
    queryFn: () => client.getBalancesByCurrency(holder, currency),
    // Negative holders (system accounts) are valid; only 0 means "no account".
    enabled: holder !== 0 && currency !== "",
    refetchInterval: 15_000,
  });
}
