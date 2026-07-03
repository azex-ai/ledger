import { useQuery, useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";
import { useLedgerMutation } from "./use-ledger-mutation";
import { ledgerKeys } from "./keys";

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

export function useJournal(id: string) {
  const client = useLedgerClient();
  return useQuery({
    // Detail uses singular ["journal", id] so invalidation of the list
    // namespace ["ledger","journals"] (e.g. on reverse) doesn't force every
    // detail page to refetch.
    queryKey: ledgerKeys.journal(id),
    queryFn: () => client.getJournal(id),
    enabled: id !== "",
  });
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

export function useReverseJournal() {
  const client = useLedgerClient();
  return useLedgerMutation(
    ({ id, reason }: { id: string; reason: string }) =>
      client.reverseJournal(id, reason),
    ["journals"],
  );
}

export function useEntries(
  params: { holder?: number; currency_uid?: string },
  limit = 50,
) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ledgerKeys.entries(params),
    queryFn: ({ pageParam }) =>
      client.listEntries({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    // Negative holders (system accounts) are valid; `!!holder` would wrongly
    // disable holder 0 only — but be explicit so a negative holder runs too.
    enabled: params.holder !== undefined && params.holder !== 0,
  });
}
