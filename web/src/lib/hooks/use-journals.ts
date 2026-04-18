import { useQuery, useMutation, useInfiniteQuery, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useJournals(limit = 20) {
  return useInfiniteQuery({
    queryKey: ["journals"],
    queryFn: ({ pageParam }) => api.listJournals({ cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

export function useJournal(id: number) {
  return useQuery({
    queryKey: ["journals", id],
    queryFn: () => api.getJournal(id),
    enabled: id > 0,
  });
}

export function usePostJournal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.postJournal,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["journals"] }),
  });
}

export function usePostTemplateJournal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Parameters<typeof api.postTemplateJournal>[0]) =>
      api.postTemplateJournal(body),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["journals"] }),
  });
}

export function useReverseJournal() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, reason }: { id: number; reason: string }) =>
      api.reverseJournal(id, reason),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["journals"] }),
  });
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
