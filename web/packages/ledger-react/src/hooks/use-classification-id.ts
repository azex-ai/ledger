import { useQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import { ledgerKeys } from "./keys";

/**
 * Result of resolving a classification code to its uid. `uid === ""` is
 * ambiguous on its own (loading vs. not-found vs. failed) — callers that
 * gate a downstream query on `uid !== ""` MUST also fold `isLoading` /
 * `isError` into their own loading/error state, or a failed classification
 * lookup silently degrades an `enabled: false` query into a false "empty"
 * result (M2, 2026-08-26 web audit) instead of surfacing as an error.
 */
export interface ClassificationLookup {
  uid: string;
  isLoading: boolean;
  isError: boolean;
  refetch: () => void;
}

/**
 * Resolve the classification ID for a given code (e.g. "deposit", "withdraw").
 *
 * The classification list is small and stable, so it's cached for a long time.
 * `uid` is "" (falsy) until classifications have loaded, weren't found, or the
 * lookup failed — `isLoading`/`isError` disambiguate those. Internal helper —
 * shared by the deposit/withdrawal/sweep hooks, not part of the public
 * package surface.
 */
export function useClassificationIdByCode(code: string): ClassificationLookup {
  const client = useLedgerClient();
  const { data, isLoading, isError, refetch } = useQuery({
    // Shares the cache with useClassifications(true) — same key on purpose.
    queryKey: ledgerKeys.classifications(true),
    queryFn: () => client.listClassifications(true),
    staleTime: 5 * 60_000,
  });
  return {
    uid: data?.find((c) => c.code === code)?.uid ?? "",
    isLoading,
    isError,
    refetch: () => void refetch(),
  };
}
