import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";
import { ledgerKeys, ledgerKeyPrefix } from "./keys";

// ─── Classifications ─────────────────────────────────────────────────

export function useClassifications(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.classifications(activeOnly),
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
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.classifications }),
  });
}

export function useDeactivateClassification() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => client.deactivateClassification(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.classifications }),
  });
}

// ─── Journal Types ───────────────────────────────────────────────────

export function useJournalTypes(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.journalTypes(activeOnly),
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
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.journalTypes }),
  });
}

export function useDeactivateJournalType() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => client.deactivateJournalType(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.journalTypes }),
  });
}

// ─── Templates ───────────────────────────────────────────────────────

export function useTemplates(activeOnly?: boolean) {
  const client = useLedgerClient();
  return useQuery({
    queryKey: ledgerKeys.templates(activeOnly),
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
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.templates }),
  });
}

export function useDeactivateTemplate() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => client.deactivateTemplate(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.templates }),
  });
}

export function usePreviewTemplate() {
  const client = useLedgerClient();
  return useMutation({
    mutationFn: ({
      code,
      ...params
    }: { code: string; holder_id: number; currency_uid: string } & Record<
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
    queryKey: ledgerKeys.currencies(activeOnly),
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
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.currencies }),
  });
}

export function useDeactivateCurrency() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (id: string) => client.deactivateCurrency(id),
    onSuccess: () =>
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.currencies }),
  });
}
