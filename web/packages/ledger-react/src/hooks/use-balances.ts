import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";

export function useBalances(holder: number) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "balances", holder],
    queryFn: () => client.getBalances(holder),
    enabled: holder > 0,
    refetchInterval: 15_000,
  });
}

export function useBalancesByCurrency(holder: number, currency: number) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "balances", holder, currency],
    queryFn: () => client.getBalancesByCurrency(holder, currency),
    enabled: holder > 0 && currency > 0,
    refetchInterval: 15_000,
  });
}
