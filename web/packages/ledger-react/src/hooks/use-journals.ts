import { useQuery, useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";
import { useLedgerMutation } from "./use-ledger-mutation";
import { ledgerKeys, ledgerKeyPrefix } from "./keys";

export function useJournals(limit = 20) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ledgerKeys.journals(limit),
    queryFn: ({ pageParam }) =>
      client.listJournals({ cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

/**
 * `id` is required — an empty string disables the query rather than firing
 * a request against a nonexistent journal. `isDisabled` (J-18, 2026-09-02 web
 * audit) makes that gating explicit: without it, `id: ""` produces the same
 * `isLoading: false, isError: false, data: undefined` shape as "fetched, no
 * such journal", indistinguishable to a headless consumer.
 */
export function useJournal(id: string) {
  const client = useLedgerClient();
  const isDisabled = id === "";
  const query = useQuery({
    // Detail uses singular ["journal", id] so invalidation of the list
    // namespace ["ledger","journals"] (e.g. on reverse) doesn't force every
    // detail page to refetch.
    queryKey: ledgerKeys.journal(id),
    queryFn: () => client.getJournal(id),
    enabled: !isDisabled,
  });
  return { ...query, isDisabled };
}

export function usePostJournal() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (body: Parameters<LedgerClient["postJournal"]>[0]) =>
      client.postJournal(body),
    ["journals"],
  );
}

export function usePostTemplateJournal() {
  const client = useLedgerClient();
  return useLedgerMutation(
    (body: Parameters<LedgerClient["postTemplateJournal"]>[0]) =>
      client.postTemplateJournal(body),
    ["journals"],
  );
}

/**
 * Not a `useLedgerMutation` (J-15, 2026-09-02 web audit): that wrapper's
 * whole purpose is minting a stable client-chosen idempotency key across
 * retries, but `client.reverseJournal` deliberately sends none — the server
 * derives its own key from the journal uid + reason and 400s if one is
 * supplied. Duplicate reversal attempts are deduped server-side via
 * `journals.reversal_of`'s partial unique index instead. Using
 * `useLedgerMutation` here would silently mint and discard a key nothing
 * ever consumes, misleading the next reader into thinking it's in effect.
 */
export function useReverseJournal() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, reason }: { id: string; reason: string }) =>
      client.reverseJournal(id, reason),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.journals });
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.balances });
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.systemBalances });
    },
  });
}

/**
 * `holder` is required — undefined/0 disables the query. `isDisabled` (J-18,
 * 2026-09-02 web audit) makes that gating explicit: without it, a missing
 * `holder` produces the same `isLoading: false, isError: false, data:
 * undefined` shape as "fetched, no entries", indistinguishable to a headless
 * consumer.
 */
export function useEntries(
  params: { holder?: number; currency_uid?: string },
  limit = 50,
) {
  const client = useLedgerClient();
  // Negative holders (system accounts) are valid; `!!holder` would wrongly
  // disable holder 0 only — but be explicit so a negative holder runs too.
  const isDisabled = params.holder === undefined || params.holder === 0;
  const query = useInfiniteQuery({
    queryKey: ledgerKeys.entries(params),
    queryFn: ({ pageParam }) =>
      client.listEntries({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: !isDisabled,
  });
  return { ...query, isDisabled };
}
