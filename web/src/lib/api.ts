const BASE_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";

export interface ApiError {
  code: string;
  message: string;
}

export class ApiRequestError extends Error {
  constructor(
    public status: number,
    public apiError: ApiError,
  ) {
    super(apiError.message);
    this.name = "ApiRequestError";
  }
}

async function request<T>(
  path: string,
  init?: RequestInit,
): Promise<T> {
  const res = await fetch(`${BASE_URL}${path}`, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      ...init?.headers,
    },
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({
      error: { code: "unknown", message: res.statusText },
    }));
    throw new ApiRequestError(res.status, body.error ?? { code: "unknown", message: res.statusText });
  }

  if (res.status === 204) return undefined as T;
  return res.json();
}

// ─── Types ───────────────────────────────────────────────────────────

export interface Journal {
  id: number;
  journal_type_id: number;
  idempotency_key: string;
  total_debit: string;
  total_credit: string;
  metadata: Record<string, string>;
  actor_id?: number;
  source: string;
  reversal_of?: number;
  created_at: string;
}

export interface Entry {
  id: number;
  journal_id: number;
  account_holder: number;
  currency_id: number;
  classification_id: number;
  entry_type: "debit" | "credit";
  amount: string;
  created_at: string;
}

export interface JournalWithEntries {
  journal: Journal;
  entries: Entry[];
}

export interface Balance {
  account_holder: number;
  currency_id: number;
  classification_id: number;
  balance: string;
}

export interface Reservation {
  id: number;
  account_holder: number;
  currency_id: number;
  reserved_amount: string;
  settled_amount?: string;
  status: "active" | "settling" | "settled" | "released";
  journal_id?: number;
  idempotency_key: string;
  expires_at: string;
  created_at: string;
  updated_at: string;
}

export interface Deposit {
  id: number;
  account_holder: number;
  currency_id: number;
  expected_amount: string;
  actual_amount?: string;
  status: "pending" | "confirming" | "confirmed" | "failed" | "expired";
  channel_name: string;
  channel_ref?: string;
  journal_id?: number;
  idempotency_key: string;
  metadata: Record<string, string>;
  expires_at?: string;
  created_at: string;
  updated_at: string;
}

export interface Withdrawal {
  id: number;
  account_holder: number;
  currency_id: number;
  amount: string;
  status: "locked" | "reserved" | "reviewing" | "processing" | "confirmed" | "failed" | "expired";
  channel_name: string;
  channel_ref?: string;
  reservation_id?: number;
  journal_id?: number;
  idempotency_key: string;
  metadata: Record<string, string>;
  review_required: boolean;
  expires_at?: string;
  created_at: string;
  updated_at: string;
}

export interface Classification {
  id: number;
  code: string;
  name: string;
  normal_side: "debit" | "credit";
  is_system: boolean;
  is_active: boolean;
  created_at: string;
}

export interface JournalType {
  id: number;
  code: string;
  name: string;
  is_active: boolean;
  created_at: string;
}

export interface TemplateLine {
  id: number;
  classification_id: number;
  entry_type: "debit" | "credit";
  holder_role: "user" | "system";
  amount_key: string;
  sort_order: number;
}

export interface EntryTemplate {
  id: number;
  code: string;
  name: string;
  journal_type_id: number;
  is_active: boolean;
  lines: TemplateLine[];
  created_at: string;
}

export interface Currency {
  id: number;
  code: string;
  name: string;
}

export interface HealthStatus {
  status: string;
  rollup_queue_depth: number;
  checkpoint_max_age_seconds: number;
  active_reservations: number;
}

export interface SystemBalance {
  currency_id: number;
  classification_id: number;
  total_balance: string;
  updated_at: string;
}

export interface ReconcileResult {
  balanced: boolean;
  gap: string;
  details?: Array<{
    account_holder: number;
    currency_id: number;
    classification_id: number;
    expected: string;
    actual: string;
    drift: string;
  }>;
  checked_at: string;
}

export interface Snapshot {
  account_holder: number;
  currency_id: number;
  classification_id: number;
  snapshot_date: string;
  balance: string;
}

export interface PaginatedResponse<T> {
  data: T[];
  next_cursor: string;
}

export interface PreviewResult {
  entries: Array<{
    account_holder: number;
    currency_id: number;
    classification_id: number;
    entry_type: "debit" | "credit";
    amount: string;
  }>;
  total_debit: string;
  total_credit: string;
}

// ─── API Functions ───────────────────────────────────────────────────

function qs(params: Record<string, string | number | boolean | undefined>): string {
  const entries = Object.entries(params).filter(([, v]) => v !== undefined && v !== "");
  if (entries.length === 0) return "";
  return "?" + new URLSearchParams(entries.map(([k, v]) => [k, String(v)])).toString();
}

// System
export const getHealth = () => request<HealthStatus>("/api/v1/system/health");
export const getSystemBalances = () => request<{ data: SystemBalance[] }>("/api/v1/system/balances");

// Journals
export const listJournals = (params: { cursor?: string; limit?: number }) =>
  request<PaginatedResponse<Journal>>(`/api/v1/journals${qs(params)}`);

export const getJournal = (id: number) =>
  request<JournalWithEntries>(`/api/v1/journals/${id}`);

export const postJournal = (body: {
  journal_type_id: number;
  idempotency_key: string;
  entries: Array<{
    account_holder: number;
    currency_id: number;
    classification_id: number;
    entry_type: "debit" | "credit";
    amount: string;
  }>;
  source?: string;
  metadata?: Record<string, string>;
}) => request<Journal>("/api/v1/journals", { method: "POST", body: JSON.stringify(body) });

export const postTemplateJournal = (body: {
  template_code: string;
  holder_id: number;
  currency_id: number;
  idempotency_key: string;
  amounts: Record<string, string>;
  source?: string;
}) => request<Journal>("/api/v1/journals/template", { method: "POST", body: JSON.stringify(body) });

export const reverseJournal = (id: number, reason: string) =>
  request<Journal>(`/api/v1/journals/${id}/reverse`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });

// Entries
export const listEntries = (params: { holder?: number; currency_id?: number; cursor?: string; limit?: number }) =>
  request<PaginatedResponse<Entry>>(`/api/v1/entries${qs(params)}`);

// Balances
export const getBalances = (holder: number) =>
  request<{ data: Balance[] }>(`/api/v1/balances/${holder}`);

export const getBalancesByCurrency = (holder: number, currency: number) =>
  request<{ data: Balance[] }>(`/api/v1/balances/${holder}/${currency}`);

export const batchBalances = (holderIds: number[], currencyId: number) =>
  request<Record<string, Balance[]>>("/api/v1/balances/batch", {
    method: "POST",
    body: JSON.stringify({ holder_ids: holderIds, currency_id: currencyId }),
  });

// Reservations
export const listReservations = (params: { holder?: number; status?: string; cursor?: string; limit?: number }) =>
  request<PaginatedResponse<Reservation>>(`/api/v1/reservations${qs(params)}`);

export const createReservation = (body: {
  account_holder: number;
  currency_id: number;
  amount: string;
  idempotency_key: string;
  expires_in?: string;
}) => request<Reservation>("/api/v1/reservations", { method: "POST", body: JSON.stringify(body) });

export const settleReservation = (id: number, actualAmount: string) =>
  request<void>(`/api/v1/reservations/${id}/settle`, {
    method: "POST",
    body: JSON.stringify({ actual_amount: actualAmount }),
  });

export const releaseReservation = (id: number) =>
  request<void>(`/api/v1/reservations/${id}/release`, { method: "POST" });

// Deposits
export const listDeposits = (params: { holder?: number; status?: string; cursor?: string; limit?: number }) =>
  request<PaginatedResponse<Deposit>>(`/api/v1/deposits${qs(params)}`);

export const createDeposit = (body: {
  account_holder: number;
  currency_id: number;
  expected_amount: string;
  channel_name: string;
  idempotency_key: string;
  metadata?: Record<string, string>;
}) => request<Deposit>("/api/v1/deposits", { method: "POST", body: JSON.stringify(body) });

export const confirmingDeposit = (id: number, channelRef: string) =>
  request<void>(`/api/v1/deposits/${id}/confirming`, {
    method: "POST",
    body: JSON.stringify({ channel_ref: channelRef }),
  });

export const confirmDeposit = (id: number, body: { actual_amount: string; channel_ref: string }) =>
  request<void>(`/api/v1/deposits/${id}/confirm`, {
    method: "POST",
    body: JSON.stringify(body),
  });

export const failDeposit = (id: number, reason: string) =>
  request<void>(`/api/v1/deposits/${id}/fail`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });

// Withdrawals
export const listWithdrawals = (params: { holder?: number; status?: string; cursor?: string; limit?: number }) =>
  request<PaginatedResponse<Withdrawal>>(`/api/v1/withdrawals${qs(params)}`);

export const createWithdrawal = (body: {
  account_holder: number;
  currency_id: number;
  amount: string;
  channel_name: string;
  idempotency_key: string;
  review_required?: boolean;
}) => request<Withdrawal>("/api/v1/withdrawals", { method: "POST", body: JSON.stringify(body) });

export const reserveWithdraw = (id: number) =>
  request<void>(`/api/v1/withdrawals/${id}/reserve`, { method: "POST" });

export const reviewWithdraw = (id: number, approved: boolean) =>
  request<void>(`/api/v1/withdrawals/${id}/review`, {
    method: "POST",
    body: JSON.stringify({ approved }),
  });

export const processWithdraw = (id: number, channelRef: string) =>
  request<void>(`/api/v1/withdrawals/${id}/process`, {
    method: "POST",
    body: JSON.stringify({ channel_ref: channelRef }),
  });

export const confirmWithdraw = (id: number) =>
  request<void>(`/api/v1/withdrawals/${id}/confirm`, { method: "POST" });

export const failWithdraw = (id: number, reason: string) =>
  request<void>(`/api/v1/withdrawals/${id}/fail`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });

export const retryWithdraw = (id: number) =>
  request<void>(`/api/v1/withdrawals/${id}/retry`, { method: "POST" });

// Classifications
export const listClassifications = (activeOnly?: boolean) =>
  request<Classification[]>(`/api/v1/classifications${qs({ active_only: activeOnly })}`);

export const createClassification = (body: {
  code: string;
  name: string;
  normal_side: "debit" | "credit";
  is_system: boolean;
}) => request<Classification>("/api/v1/classifications", { method: "POST", body: JSON.stringify(body) });

export const deactivateClassification = (id: number) =>
  request<void>(`/api/v1/classifications/${id}/deactivate`, { method: "POST" });

// Journal Types
export const listJournalTypes = (activeOnly?: boolean) =>
  request<JournalType[]>(`/api/v1/journal-types${qs({ active_only: activeOnly })}`);

export const createJournalType = (body: { code: string; name: string }) =>
  request<JournalType>("/api/v1/journal-types", { method: "POST", body: JSON.stringify(body) });

export const deactivateJournalType = (id: number) =>
  request<void>(`/api/v1/journal-types/${id}/deactivate`, { method: "POST" });

// Templates
export const listTemplates = (activeOnly?: boolean) =>
  request<EntryTemplate[]>(`/api/v1/templates${qs({ active_only: activeOnly })}`);

export const createTemplate = (body: {
  code: string;
  name: string;
  journal_type_id: number;
  lines: Array<{
    classification_id: number;
    entry_type: "debit" | "credit";
    holder_role: "user" | "system";
    amount_key: string;
    sort_order: number;
  }>;
}) => request<EntryTemplate>("/api/v1/templates", { method: "POST", body: JSON.stringify(body) });

export const deactivateTemplate = (id: number) =>
  request<void>(`/api/v1/templates/${id}/deactivate`, { method: "POST" });

export const previewTemplate = (code: string, params: { holder_id: number; currency_id: number } & Record<string, string | number>) =>
  request<PreviewResult>(`/api/v1/templates/${code}/preview`, {
    method: "POST",
    body: JSON.stringify(params),
  });

// Currencies
export const listCurrencies = () => request<Currency[]>("/api/v1/currencies");

export const createCurrency = (body: { code: string; name: string }) =>
  request<Currency>("/api/v1/currencies", { method: "POST", body: JSON.stringify(body) });

// Reconciliation
export const reconcileGlobal = () =>
  request<ReconcileResult>("/api/v1/reconcile", { method: "POST" });

export const reconcileAccount = (holder: number, currencyId: number) =>
  request<ReconcileResult>("/api/v1/reconcile/account", {
    method: "POST",
    body: JSON.stringify({ holder, currency_id: currencyId }),
  });

// Snapshots
export const listSnapshots = (params: {
  holder?: number;
  currency_id?: number;
  start?: string;
  end?: string;
}) => request<{ data: Snapshot[] }>(`/api/v1/snapshots${qs(params)}`);
