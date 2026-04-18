import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as api from "@/lib/api";

// ─── Classifications ─────────────────────────────────────────────────

export function useClassifications(activeOnly?: boolean) {
  return useQuery({
    queryKey: ["classifications", activeOnly],
    queryFn: () => api.listClassifications(activeOnly),
  });
}

export function useCreateClassification() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createClassification,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["classifications"] }),
  });
}

export function useDeactivateClassification() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.deactivateClassification,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["classifications"] }),
  });
}

// ─── Journal Types ───────────────────────────────────────────────────

export function useJournalTypes(activeOnly?: boolean) {
  return useQuery({
    queryKey: ["journal-types", activeOnly],
    queryFn: () => api.listJournalTypes(activeOnly),
  });
}

export function useCreateJournalType() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createJournalType,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["journal-types"] }),
  });
}

export function useDeactivateJournalType() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.deactivateJournalType,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["journal-types"] }),
  });
}

// ─── Templates ───────────────────────────────────────────────────────

export function useTemplates(activeOnly?: boolean) {
  return useQuery({
    queryKey: ["templates", activeOnly],
    queryFn: () => api.listTemplates(activeOnly),
  });
}

export function useCreateTemplate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createTemplate,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["templates"] }),
  });
}

export function useDeactivateTemplate() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.deactivateTemplate,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["templates"] }),
  });
}

export function usePreviewTemplate() {
  return useMutation({
    mutationFn: ({ code, ...params }: { code: string; holder_id: number; currency_id: number } & Record<string, string | number>) =>
      api.previewTemplate(code, params as Parameters<typeof api.previewTemplate>[1]),
  });
}

// ─── Currencies ──────────────────────────────────────────────────────

export function useCurrencies() {
  return useQuery({
    queryKey: ["currencies"],
    queryFn: api.listCurrencies,
  });
}

export function useCreateCurrency() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: api.createCurrency,
    onSuccess: () => qc.invalidateQueries({ queryKey: ["currencies"] }),
  });
}
