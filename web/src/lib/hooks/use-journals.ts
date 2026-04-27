import { useQuery, useInfiniteQuery } from "@tanstack/react-query";
import { useLedgerMutation } from "./use-ledger-mutation";
import * as api from "@/lib/api";

export function useJournals(limit = 20) {
  return useInfiniteQuery({
    queryKey: ["journals", limit],
    queryFn: ({ pageParam }) => api.listJournals({ cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

export function useJournal(id: number) {
  return useQuery({
    // Detail uses singular ["journal", id] so invalidation of the list
    // namespace ["journals"] (e.g. on reverse) doesn't force every detail
    // page to refetch.
    queryKey: ["journal", id],
    queryFn: () => api.getJournal(id),
    enabled: id > 0,
  });
}

export function usePostJournal() {
  return useLedgerMutation(api.postJournal, ["journals"]);
}

export function usePostTemplateJournal() {
  return useLedgerMutation(
    (body: Parameters<typeof api.postTemplateJournal>[0]) =>
      api.postTemplateJournal(body),
    ["journals"],
  );
}

export function useReverseJournal() {
  return useLedgerMutation(
    ({ id, reason }: { id: number; reason: string }) =>
      api.reverseJournal(id, reason),
    ["journals"],
  );
}

export function useEntries(params: { holder?: number; currency_id?: number }, limit = 50) {
  return useInfiniteQuery({
    queryKey: ["entries", params],
    queryFn: ({ pageParam }) => api.listEntries({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
    enabled: !!params.holder,
  });
}
