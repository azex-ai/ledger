import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";

// ─── Classifications ─────────────────────────────────────────────────

export function useClassifications(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "classifications", activeOnly],
    queryFn: () => client.listClassifications(activeOnly),
  });
}

export function useCreateClassification() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createClassification"]>[0]) =>
      client.createClassification(body),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "classifications"] }),
  });
}

export function useDeactivateClassification() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => client.deactivateClassification(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "classifications"] }),
  });
}

// ─── Journal Types ───────────────────────────────────────────────────

export function useJournalTypes(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "journal-types", activeOnly],
    queryFn: () => client.listJournalTypes(activeOnly),
  });
}

export function useCreateJournalType() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createJournalType"]>[0]) =>
      client.createJournalType(body),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "journal-types"] }),
  });
}

export function useDeactivateJournalType() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => client.deactivateJournalType(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "journal-types"] }),
  });
}

// ─── Templates ───────────────────────────────────────────────────────

export function useTemplates(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "templates", activeOnly],
    queryFn: () => client.listTemplates(activeOnly),
  });
}

export function useCreateTemplate() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createTemplate"]>[0]) =>
      client.createTemplate(body),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "templates"] }),
  });
}

export function useDeactivateTemplate() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => client.deactivateTemplate(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "templates"] }),
  });
}

export function usePreviewTemplate() {
  const client = useLedgerClient();
  return useMutation({
    mutationFn: ({
      code,
      ...params
    }: { code: string; holder_id: number; currency_id: number } & Record<
      string,
      string | number
    >) =>
      client.previewTemplate(
        code,
        params as Parameters<LedgerClient["previewTemplate"]>[1],
      ),
  });
}

// ─── Currencies ──────────────────────────────────────────────────────

export function useCurrencies(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ["ledger", "currencies", activeOnly],
    queryFn: () => client.listCurrencies(activeOnly),
  });
}

export function useCreateCurrency() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createCurrency"]>[0]) =>
      client.createCurrency(body),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "currencies"] }),
  });
}

export function useDeactivateCurrency() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => client.deactivateCurrency(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ["ledger", "currencies"] }),
  });
}
