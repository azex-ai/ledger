import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";

export function useWithdrawals(params: { holder?: number; status?: string }) {
  return useQuery({
    queryKey: ["withdrawals", params],
    queryFn: () => api.listWithdrawals(params),
  });
}

export function useReserveWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.reserveWithdraw(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}

export function useReviewWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, approved }: { id: number; approved: boolean }) =>
      api.reviewWithdraw(id, approved),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}

export function useProcessWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, channelRef }: { id: number; channelRef: string }) =>
      api.processWithdraw(id, channelRef),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}

export function useConfirmWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.confirmWithdraw(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}

export function useFailWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: ({ id, reason }: { id: number; reason: string }) =>
      api.failWithdraw(id, reason),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}

export function useRetryWithdraw() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.retryWithdraw(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["withdrawals"] }),
  });
}
