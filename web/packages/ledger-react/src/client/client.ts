import type {
  ApiError,
  Balance,
  Booking,
  Classification,
  CreateBookingBody,
  Currency,
  Entry,
  EntryTemplate,
  Event,
  HealthStatus,
  Journal,
  JournalType,
  JournalWithEntries,
  ListBookingsParams,
  PaginatedResponse,
  PreviewResult,
  ReconcileResult,
  Reservation,
  Snapshot,
  SystemBalance,
  TransitionBookingBody,
} from "./types";

export class ApiRequestError extends Error {
  constructor(
    public status: number,
    public apiError: ApiError,
  ) {
    super(apiError.message);
    this.name = "ApiRequestError";
  }
}

export interface LedgerClientConfig {
  baseUrl: string;
  apiKey?: string;
  /**
   * Optional fetch override (server use / tests). MUST be a STABLE reference
   * (module-level or `useCallback`'d). LedgerProvider keys its client `useMemo`
   * on this field, so an inline arrow (`fetch: (...) => ...`) changes identity
   * every render and rebuilds the client — re-rendering all consumers.
   */
  fetch?: typeof fetch;
}

interface Envelope<T> {
  code: number;
  message: string;
  data: T;
}

const MUTATING_METHODS = new Set(["POST", "PUT", "PATCH", "DELETE"]);

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

export function createLedgerClient(config: LedgerClientConfig) {
  async function request<T>(path: string, init?: RequestInit): Promise<T> {
    // Resolve the fetch implementation per call: an explicit override wins,
    // otherwise the ambient globalThis.fetch (read lazily so test doubles /
    // MSW installed after client construction are still picked up).
    const fetchImpl = config.fetch ?? globalThis.fetch;
    const method = (init?.method ?? "GET").toUpperCase();
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(init?.headers as Record<string, string> | undefined),
    };
    if (MUTATING_METHODS.has(method) && config.apiKey) {
      headers["Authorization"] = `Bearer ${config.apiKey}`;
    }

    const res = await fetchImpl(`${config.baseUrl}${path}`, {
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

  return {
    // System
    getHealth: () => request<HealthStatus>("/api/v1/system/health"),
    getSystemBalances: () =>
      request<SystemBalance[]>("/api/v1/system/balances"),

    // Journals
    listJournals: (params: { cursor?: string; limit?: number }) =>
      request<PaginatedResponse<Journal>>(`/api/v1/journals${qs(params)}`),

    getJournal: (id: number) =>
      request<JournalWithEntries>(`/api/v1/journals/${id}`),

    postJournal: (body: {
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
      }),

    postTemplateJournal: (body: {
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
      }),

    reverseJournal: (id: number, reason: string) =>
      request<Journal>(`/api/v1/journals/${id}/reverse`, {
        method: "POST",
        body: JSON.stringify({ reason }),
      }),

    // Entries
    listEntries: (params: {
      holder?: number;
      currency_id?: number;
      cursor?: string;
      limit?: number;
    }) => request<PaginatedResponse<Entry>>(`/api/v1/entries${qs(params)}`),

    // Balances
    getBalances: (holder: number) =>
      request<Balance[]>(`/api/v1/balances/${holder}`),

    getBalancesByCurrency: (holder: number, currency: number) =>
      request<Balance[]>(`/api/v1/balances/${holder}/${currency}`),

    batchBalances: (holderIds: number[], currencyId: number) =>
      request<Record<string, Balance[]>>("/api/v1/balances/batch", {
        method: "POST",
        body: JSON.stringify({
          holder_ids: holderIds,
          currency_id: currencyId,
        }),
      }),

    // Reservations
    listReservations: (params: {
      holder?: number;
      status?: string;
      limit?: number;
    }) => request<Reservation[]>(`/api/v1/reservations${qs(params)}`),

    createReservation: (body: {
      account_holder: number;
      currency_id: number;
      amount: string;
      idempotency_key: string;
      expires_in?: string;
    }) =>
      request<Reservation>("/api/v1/reservations", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    settleReservation: (id: number, actualAmount: string) =>
      request<void>(`/api/v1/reservations/${id}/settle`, {
        method: "POST",
        body: JSON.stringify({ actual_amount: actualAmount }),
      }),

    releaseReservation: (id: number) =>
      request<void>(`/api/v1/reservations/${id}/release`, { method: "POST" }),

    // Bookings (unified — replaces v1 deposits + withdrawals)
    createBooking: (body: CreateBookingBody) =>
      request<Booking>("/api/v1/bookings", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    transitionBooking: (id: number, body: TransitionBookingBody) =>
      request<Event>(`/api/v1/bookings/${id}/transition`, {
        method: "POST",
        body: JSON.stringify(body),
      }),

    getBooking: (id: number) => request<Booking>(`/api/v1/bookings/${id}`),

    listBookings: (params: ListBookingsParams) =>
      request<PaginatedResponse<Booking>>(
        `/api/v1/bookings${qs(params as Record<string, string | number | undefined>)}`,
      ),

    // Events (outbound)
    getEvent: (id: number) => request<Event>(`/api/v1/events/${id}`),

    listEvents: (params: {
      classification_code?: string;
      booking_id?: number;
      to_status?: string;
      cursor?: string;
      limit?: number;
    }) => request<PaginatedResponse<Event>>(`/api/v1/events${qs(params)}`),

    // Classifications
    listClassifications: (activeOnly?: boolean) =>
      request<Classification[]>(
        `/api/v1/classifications${qs({ active_only: activeOnly })}`,
      ),

    createClassification: (body: {
      code: string;
      name: string;
      normal_side: "debit" | "credit";
      is_system: boolean;
    }) =>
      request<Classification>("/api/v1/classifications", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    deactivateClassification: (id: number) =>
      request<void>(`/api/v1/classifications/${id}/deactivate`, {
        method: "POST",
      }),

    // Journal Types
    listJournalTypes: (activeOnly?: boolean) =>
      request<JournalType[]>(
        `/api/v1/journal-types${qs({ active_only: activeOnly })}`,
      ),

    createJournalType: (body: { code: string; name: string }) =>
      request<JournalType>("/api/v1/journal-types", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    deactivateJournalType: (id: number) =>
      request<void>(`/api/v1/journal-types/${id}/deactivate`, {
        method: "POST",
      }),

    // Templates
    listTemplates: (activeOnly?: boolean) =>
      request<EntryTemplate[]>(
        `/api/v1/templates${qs({ active_only: activeOnly })}`,
      ),

    createTemplate: (body: {
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
      }),

    deactivateTemplate: (id: number) =>
      request<void>(`/api/v1/templates/${id}/deactivate`, { method: "POST" }),

    previewTemplate: (
      code: string,
      params: { holder_id: number; currency_id: number } & Record<
        string,
        string | number
      >,
    ) =>
      request<PreviewResult>(`/api/v1/templates/${code}/preview`, {
        method: "POST",
        body: JSON.stringify(params),
      }),

    // Currencies
    listCurrencies: (activeOnly?: boolean) =>
      request<Currency[]>(`/api/v1/currencies${qs({ active_only: activeOnly })}`),

    createCurrency: (body: { code: string; name: string; exponent: number }) =>
      request<Currency>("/api/v1/currencies", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    deactivateCurrency: (id: number) =>
      request<void>(`/api/v1/currencies/${id}/deactivate`, { method: "POST" }),

    // Reconciliation
    reconcileGlobal: () =>
      request<ReconcileResult>("/api/v1/reconcile", { method: "POST" }),

    reconcileAccount: (holder: number, currencyId: number) =>
      request<ReconcileResult>("/api/v1/reconcile/account", {
        method: "POST",
        body: JSON.stringify({ holder, currency_id: currencyId }),
      }),

    // Snapshots
    listSnapshots: (params: {
      holder?: number;
      currency_id?: number;
      start?: string;
      end?: string;
    }) => request<Snapshot[]>(`/api/v1/snapshots${qs(params)}`),
  };
}

export type LedgerClient = ReturnType<typeof createLedgerClient>;
