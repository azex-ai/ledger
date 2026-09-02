import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef } from "react";
import { useLedgerClient } from "../provider/context";
import type { LedgerClient } from "../client/client";
import { ledgerKeys, ledgerKeyPrefix } from "./keys";

/**
 * Idempotency key stable across retries of one attempt sequence, cleared on
 * success — the SAME client-side lifecycle useLedgerMutation gives every
 * other mutation (api-contract.md §9). Used directly here (rather than
 * useLedgerMutation) because these mutations only need a plain
 * invalidate-on-success, one line each.
 *
 * J-14 (2026-09-02 web audit): unlike most mutations, the metadata
 * create/deactivate handlers this key is sent to (`server/handler_metadata.go`)
 * do NOT read `Idempotency-Key` at all — `middleware_idempotency.go` aliases
 * the header into the request body, but nothing in these handlers looks at
 * that field, so it's silently dropped server-side. A retried create after a
 * client-side timeout is therefore deduped by each resource's unique `code`
 * constraint (a second attempt gets a constraint-violation error, not a
 * replayed success), NOT by this key. This is documented here because a
 * server-side change to start honoring the key would be the natural place
 * to also update this comment — see the TODO note in the same audit.
 */
function useIdempotencyKey() {
  const ref = useRef<string | null>(null);
  return {
    get: () => {
      if (!ref.current) ref.current = crypto.randomUUID();
      return ref.current;
    },
    clear: () => {
      ref.current = null;
    },
  };
}

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
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createClassification"]>[0]) =>
      client.createClassification(body, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.classifications });
    },
  });
}

export function useDeactivateClassification() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (id: string) => client.deactivateClassification(id, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.classifications });
    },
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
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createJournalType"]>[0]) =>
      client.createJournalType(body, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.journalTypes });
    },
  });
}

export function useDeactivateJournalType() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (id: string) => client.deactivateJournalType(id, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.journalTypes });
    },
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
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createTemplate"]>[0]) =>
      client.createTemplate(body, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.templates });
    },
  });
}

export function useDeactivateTemplate() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (id: string) => client.deactivateTemplate(id, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.templates });
    },
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
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (body: Parameters<LedgerClient["createCurrency"]>[0]) =>
      client.createCurrency(body, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.currencies });
    },
  });
}

export function useDeactivateCurrency() {
  const client = useLedgerClient();
  const qc = useQueryClient();
  const idempotency = useIdempotencyKey();
  return useMutation({
    mutationFn: (id: string) => client.deactivateCurrency(id, idempotency.get()),
    onSuccess: () => {
      idempotency.clear();
      qc.invalidateQueries({ queryKey: ledgerKeyPrefix.currencies });
    },
  });
}

/**
 * uid → human code lookups for chart/axis labels. Metadata lists are small
 * and cached; while they load, fall back to a shortened uid. Shared by every
 * skin's dashboard so the fallback behavior can't drift between them.
 */
export function useUidCodeLookups() {
  const { data: classifications } = useClassifications();
  const { data: currencies } = useCurrencies();
  const classCode = (uid: string) =>
    classifications?.find((c) => c.uid === uid)?.code ?? uid.slice(0, 8);
  const currencyCode = (uid: string) =>
    currencies?.find((c) => c.uid === uid)?.code ?? uid.slice(0, 8);
  return { classCode, currencyCode };
}
