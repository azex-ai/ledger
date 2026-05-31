import { useQuery, useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";
import { useLedgerMutation } from "./use-ledger-mutation";

export function useJournals(limit = 20) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ["ledger", "journals", limit],
    queryFn: ({ pageParam }) =>
      client.listJournals({ cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

export function useJournal(id: number) {
  const client = useLedgerClient();
  return useQuery({
    // Detail uses singular ["journal", id] so invalidation of the list
    // namespace ["ledger","journals"] (e.g. on reverse) doesn't force every
    // detail page to refetch.
    queryKey: ["ledger", "journal", id],
    queryFn: () => client.getJournal(id),
    enabled: id > 0,
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
    ({ id, reason }: { id: number; reason: string }) =>
      client.reverseJournal(id, reason),
    ["journals"],
  );
}

export function useEntries(
  params: { holder?: number; currency_id?: number },
  limit = 50,
) {
  const client = useLedgerClient();
  return useInfiniteQuery({
    queryKey: ["ledger", "entries", params],
    queryFn: ({ pageParam }) =>
      client.listEntries({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: !!params.holder,
  });
}
