import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useReservations(params: { holder?: number; status?: string }, limit = 20) {
  return useInfiniteQuery({
    queryKey: ["reservations", params],
    queryFn: ({ pageParam }) => api.listReservations({ ...params, cursor: pageParam, limit }),
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.next_cursor || undefined,
  });
}

export function useSettleReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, actualAmount }: { id: number; actualAmount: string }) =>
      api.settleReservation(id, actualAmount),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["reservations"] }),
  });
}

export function useReleaseReservation() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.releaseReservation(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["reservations"] }),
  });
}
