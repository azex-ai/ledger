// In production, NEXT_PUBLIC_API_URL must be configured explicitly so the
// dashboard never silently calls localhost. In development the fallback
// keeps the local workflow zero-config. The check fires at the first API
// call (not module-load) so Next.js can still prerender static pages
// during the production build with no API key configured.
const BASE_URL = process.env.NEXT_PUBLIC_API_URL ?? "http://localhost:8080";
const API_KEY = process.env.NEXT_PUBLIC_API_KEY ?? "";

function assertProductionConfig(): void {
  if (
    !process.env.NEXT_PUBLIC_API_URL &&
    process.env.NODE_ENV === "production" &&
    typeof window !== "undefined"
  ) {
    throw new Error(
      "NEXT_PUBLIC_API_URL must be set in production builds (no localhost fallback)",
    );
  }
}

export interface ApiError {
  code: number;
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

interface Envelope<T> {
  code: number;
  message: string;
  data: T;
}

const MUTATING_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  assertProductionConfig();
  const method = (init?.method ?? "GET").toUpperCase();
  const headers: Record<string, string> = {
    "Content-Type": "application/json",
    ...(init?.headers as Record<string, string> | undefined),
  };
  if (MUTATING_METHODS.has(method) && API_KEY) {
    headers["Authorization"] = `Bearer ${API_KEY}`;
  }

  const res = await fetch(`${BASE_URL}${path}`, {
    ...init,
    headers,
  });

  if (!res.ok) {
    const body = await res.json().catch(() => ({
      code: 19999,
      message: res.statusText,
    }));
    throw new ApiRequestError(res.status, {
      code: body.code ?? 19999,
      message: body.message ?? res.statusText,
    });
  }

  if (res.status === 204) return undefined as T;

  const envelope: Envelope<T> = await res.json();
  return envelope.data;
}

// ─── Types ───────────────────────────────────────────────────────────

export interface Journal {
  id: number;
  journal_type_id: number;
  idempotency_key: string;
  total_debit: string;
  total_credit: string;
  metadata: Record<string, unknown>;
  actor_id: number;
  source: string;
  reversal_of: number | null;
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
  settled_amount: string;
  status: "active" | "settling" | "settled" | "released";
  journal_id: number | null;
  idempotency_key: string;
  expires_at: string;
  created_at: string;
  updated_at: string;
}

export interface Lifecycle {
  initial: string;
  terminal: string[];
  transitions: Record<string, string[]>;
}

// Booking is the unified record replacing v1 Deposit/Withdrawal.
// Its lifecycle is governed by the classification.
export interface Booking {
  id: number;
  classification_id: number;
  account_holder: number;
  currency_id: number;
  amount: string;
  settled_amount: string;
  status: string;
  channel_name: string;
  channel_ref: string;
  // ReservationID and JournalID are nullable on the backend (NULL means
  // not yet linked). The remaining fields are NOT NULL.
  reservation_id: number | null;
  journal_id: number | null;
  idempotency_key: string;
  metadata: Record<string, unknown>;
  expires_at: string;
  created_at: string;
  updated_at: string;
}

export interface Event {
  id: number;
  classification_code: string;
  booking_id: number;
  account_holder: number;
  currency_id: number;
  from_status: string;
  to_status: string;
  amount: string;
  settled_amount: string;
  journal_id: number | null;
  metadata: Record<string, unknown>;
  occurred_at: string;
}

export interface Classification {
  id: number;
  code: string;
  name: string;
  normal_side: "debit" | "credit";
  is_system: boolean;
  is_active: boolean;
  lifecycle: Lifecycle | null;
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
  details: Array<{
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

function qs(
  params: Record<string, string | number | boolean | undefined>,
): string {
  const entries = Object.entries(params).filter(
    ([, v]) => v !== undefined && v !== "",
  );
  if (entries.length === 0) return "";
  return (
    "?" +
    new URLSearchParams(entries.map(([k, v]) => [k, String(v)])).toString()
  );
}

// System
export const getHealth = () => request<HealthStatus>("/api/v1/system/health");
export const getSystemBalances = () =>
  request<SystemBalance[]>("/api/v1/system/balances");

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
}) =>
  request<Journal>("/api/v1/journals", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const postTemplateJournal = (body: {
  template_code: string;
  holder_id: number;
  currency_id: number;
  idempotency_key: string;
  amounts: Record<string, string>;
  source?: string;
}) =>
  request<Journal>("/api/v1/journals/template", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const reverseJournal = (id: number, reason: string) =>
  request<Journal>(`/api/v1/journals/${id}/reverse`, {
    method: "POST",
    body: JSON.stringify({ reason }),
  });

// Entries
export const listEntries = (params: {
  holder?: number;
  currency_id?: number;
  cursor?: string;
  limit?: number;
}) =>
  request<PaginatedResponse<Entry>>(`/api/v1/entries${qs(params)}`);

// Balances
export const getBalances = (holder: number) =>
  request<Balance[]>(`/api/v1/balances/${holder}`);

export const getBalancesByCurrency = (holder: number, currency: number) =>
  request<Balance[]>(`/api/v1/balances/${holder}/${currency}`);

export const batchBalances = (holderIds: number[], currencyId: number) =>
  request<Record<string, Balance[]>>("/api/v1/balances/batch", {
    method: "POST",
    body: JSON.stringify({ holder_ids: holderIds, currency_id: currencyId }),
  });

// Reservations
export const listReservations = (params: {
  holder?: number;
  status?: string;
  limit?: number;
}) =>
  request<Reservation[]>(`/api/v1/reservations${qs(params)}`);

export const createReservation = (body: {
  account_holder: number;
  currency_id: number;
  amount: string;
  idempotency_key: string;
  expires_in?: string;
}) =>
  request<Reservation>("/api/v1/reservations", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const settleReservation = (id: number, actualAmount: string) =>
  request<void>(`/api/v1/reservations/${id}/settle`, {
    method: "POST",
    body: JSON.stringify({ actual_amount: actualAmount }),
  });

export const releaseReservation = (id: number) =>
  request<void>(`/api/v1/reservations/${id}/release`, { method: "POST" });

// Bookings (unified — replaces v1 deposits + withdrawals)
export interface CreateBookingBody {
  classification_code: string;
  account_holder: number;
  currency_id: number;
  amount: string;
  idempotency_key: string;
  channel_name: string;
  metadata?: Record<string, unknown>;
  expires_at?: string;
}

export interface TransitionBookingBody {
  to_status: string;
  channel_ref?: string;
  amount?: string;
  metadata?: Record<string, unknown>;
  actor_id?: number;
}

export interface ListBookingsParams {
  holder?: number;
  classification_id?: number;
  status?: string;
  cursor?: string;
  limit?: number;
}

export const createBooking = (body: CreateBookingBody) =>
  request<Booking>("/api/v1/bookings", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const transitionBooking = (id: number, body: TransitionBookingBody) =>
  request<Event>(`/api/v1/bookings/${id}/transition`, {
    method: "POST",
    body: JSON.stringify(body),
  });

export const getBooking = (id: number) =>
  request<Booking>(`/api/v1/bookings/${id}`);

export const listBookings = (params: ListBookingsParams) =>
  request<PaginatedResponse<Booking>>(
    `/api/v1/bookings${qs(params as Record<string, string | number | undefined>)}`,
  );

// Events (outbound)
export const getEvent = (id: number) => request<Event>(`/api/v1/events/${id}`);

export const listEvents = (params: {
  classification_code?: string;
  booking_id?: number;
  to_status?: string;
  cursor?: string;
  limit?: number;
}) => request<PaginatedResponse<Event>>(`/api/v1/events${qs(params)}`);

// Classifications
export const listClassifications = (activeOnly?: boolean) =>
  request<Classification[]>(
    `/api/v1/classifications${qs({ active_only: activeOnly })}`,
  );

export const createClassification = (body: {
  code: string;
  name: string;
  normal_side: "debit" | "credit";
  is_system: boolean;
}) =>
  request<Classification>("/api/v1/classifications", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const deactivateClassification = (id: number) =>
  request<void>(`/api/v1/classifications/${id}/deactivate`, { method: "POST" });

// Journal Types
export const listJournalTypes = (activeOnly?: boolean) =>
  request<JournalType[]>(
    `/api/v1/journal-types${qs({ active_only: activeOnly })}`,
  );

export const createJournalType = (body: { code: string; name: string }) =>
  request<JournalType>("/api/v1/journal-types", {
    method: "POST",
    body: JSON.stringify(body),
  });

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
}) =>
  request<EntryTemplate>("/api/v1/templates", {
    method: "POST",
    body: JSON.stringify(body),
  });

export const deactivateTemplate = (id: number) =>
  request<void>(`/api/v1/templates/${id}/deactivate`, { method: "POST" });

export const previewTemplate = (
  code: string,
  params: { holder_id: number; currency_id: number } & Record<
    string,
    string | number
  >,
) =>
  request<PreviewResult>(`/api/v1/templates/${code}/preview`, {
    method: "POST",
    body: JSON.stringify(params),
  });

// Currencies
export const listCurrencies = () => request<Currency[]>("/api/v1/currencies");

export const createCurrency = (body: { code: string; name: string }) =>
  request<Currency>("/api/v1/currencies", {
    method: "POST",
    body: JSON.stringify(body),
  });

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
}) => request<Snapshot[]>(`/api/v1/snapshots${qs(params)}`);
