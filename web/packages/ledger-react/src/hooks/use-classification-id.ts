import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { ledgerKeys } from "./keys";

/**
 * Resolve the classification ID for a given code (e.g. "deposit", "withdraw").
 *
 * The classification list is small and stable, so it's cached for a long time.
 * Returns "" (falsy) until classifications have loaded. Internal helper — shared
 * by the deposit/withdrawal hooks, not part of the public package surface.
 */
export function useClassificationIdByCode(code: string): string {
  const client = useLedgerClient();
  const { data } = useQuery({
    // Shares the cache with useClassifications(true) — same key on purpose.
    queryKey: ledgerKeys.classifications(true),
    queryFn: () => client.listClassifications(true),
    staleTime: 5 * 60_000,
  });
  return data?.find((c) => c.code === code)?.uid ?? "";
}
