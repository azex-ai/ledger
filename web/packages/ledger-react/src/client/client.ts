import type {
  ApiError,
  Balance,
  BalanceBreakdown,
  BalanceByCurrency,
  Booking,
  Classification,
  CreateBookingBody,
  Currency,
  DepositAddress,
  Entry,
  EntryTemplate,
  Event,
  HealthStatus,
  HolderBalances,
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

interface ErrorMessage {
  text: string;
  fields?: Record<string, string>;
}

type Envelope<T> =
  | { code: 200; message: null; data: T }
  | { code: number; message: ErrorMessage; data: null };

function idempotencyKeyFromBody(body: BodyInit | null | undefined): string {
  if (typeof body === "string") {
    try {
      const parsed = JSON.parse(body) as { idempotency_key?: unknown };
      if (typeof parsed.idempotency_key === "string" && parsed.idempotency_key) {
        return parsed.idempotency_key;
      }
    } catch {
      // The server remains the authority for malformed JSON. The request still
      // receives a stable key for this logical attempt.
    }
  }
  return crypto.randomUUID();
}

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
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(init?.headers as Record<string, string> | undefined),
    };
    // Every endpoint requires the key when auth is configured — reads
    // included (server enforces bearer auth on the whole surface except the
    // k8s probes and the HMAC-verified webhook path).
    if (config.apiKey) {
      headers["Authorization"] = `Bearer ${config.apiKey}`;
    }
    const method = (init?.method ?? "GET").toUpperCase();
    if (
      !["GET", "HEAD", "OPTIONS"].includes(method) &&
      !headers["Idempotency-Key"]
    ) {
      headers["Idempotency-Key"] = idempotencyKeyFromBody(init?.body);
    }

    const res = await fetchImpl(`${config.baseUrl}${path}`, {
      ...init,
      headers,
      signal: init?.signal ?? AbortSignal.timeout(15_000),
    });

    if (res.status === 204) return undefined as T;

    const envelope = (await res.json().catch(() => null)) as Envelope<T> | null;
    if (!envelope || !res.ok || envelope.code !== 200) {
      const message =
        envelope?.message && typeof envelope.message === "object"
          ? envelope.message
          : { text: res.statusText || "Request failed" };
      throw new ApiRequestError(res.status, {
        code: envelope && envelope.code !== 200 ? envelope.code : 19999,
        message: message.text,
        fields: message.fields,
      });
    }
    return envelope.data as T;
  }

  return {
    // System
    getHealth: () => request<HealthStatus>("/api/v1/system/health"),
    getSystemBalances: () =>
      request<PaginatedResponse<SystemBalance>>("/api/v1/system/balances").then(
        (d) => d.list,
      ),

    // Journals
    listJournals: (params: { cursor?: string; limit?: number }) =>
      request<PaginatedResponse<Journal>>(`/api/v1/journals${qs(params)}`),

    getJournal: (id: string) =>
      request<JournalWithEntries>(`/api/v1/journals/${id}`),

    postJournal: (body: {
      journal_type_uid: string;
      idempotency_key: string;
      entries: Array<{
        account_holder: number;
        currency_uid: string;
        classification_uid: string;
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
      currency_uid: string;
      idempotency_key: string;
      amounts: Record<string, string>;
      source?: string;
    }) =>
      request<Journal>("/api/v1/journals/template", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    reverseJournal: (id: string, reason: string) =>
      request<Journal>(`/api/v1/journals/${id}/reverse`, {
        method: "POST",
        body: JSON.stringify({ reason }),
      }),

    // Entries
    listEntries: (params: {
      holder?: number;
      currency_uid?: string;
      cursor?: string;
      limit?: number;
    }) => request<PaginatedResponse<Entry>>(`/api/v1/entries${qs(params)}`),

    // Balances
    getBalances: (holder: number) =>
      request<PaginatedResponse<Balance>>(`/api/v1/balances/${holder}`).then(
        (d) => d.list,
      ),

    getBalancesByCurrency: (holder: number, currency: string) =>
      request<BalanceByCurrency>(`/api/v1/balances/${holder}/${currency}`),

    // Liquidity view: available / pending / locked / total. `available` is
    // exactly the figure Reserve enforces (INVARIANTS I-11).
    getBalanceBreakdown: (holder: number, currency: string) =>
      request<BalanceBreakdown>(
        `/api/v1/balances/${holder}/${currency}/breakdown`,
      ),

    batchBalances: (holderIds: number[], currencyUid: string) =>
      request<PaginatedResponse<HolderBalances>>("/api/v1/balances/batch", {
        method: "POST",
        body: JSON.stringify({
          holder_ids: holderIds,
          currency_uid: currencyUid,
        }),
      }).then((d) => d.list),

    // Reservations
    listReservations: (params: {
      holder?: number;
      status?: string;
      cursor?: string;
      limit?: number;
    }) =>
      request<PaginatedResponse<Reservation>>(
        `/api/v1/reservations${qs(params)}`,
      ),

    createReservation: (body: {
      account_holder: number;
      currency_uid: string;
      amount: string;
      idempotency_key: string;
      expires_in?: string;
    }) =>
      request<Reservation>("/api/v1/reservations", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    // idempotencyKey: caller-supplied, stable across retries of the same
    // attempt sequence (api-contract.md §9 — see useLedgerMutation).
    settleReservation: (id: string, actualAmount: string, idempotencyKey: string) =>
      request<void>(`/api/v1/reservations/${id}/settle`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: JSON.stringify({ actual_amount: actualAmount }),
      }),

    // Partial settlement accumulates; idempotency_key is REQUIRED (I-3) — a
    // retried request with the same key replays without double-applying.
    settlePartialReservation: (
      id: string,
      amount: string,
      idempotencyKey: string,
    ) =>
      request<void>(`/api/v1/reservations/${id}/settle-partial`, {
        method: "POST",
        body: JSON.stringify({ amount, idempotency_key: idempotencyKey }),
      }),

    finalizeReservationSettlement: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/reservations/${id}/finalize`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    releaseReservation: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/reservations/${id}/release`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    // Bookings (unified — replaces v1 deposits + withdrawals)
    createBooking: (body: CreateBookingBody) =>
      request<Booking>("/api/v1/bookings", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    transitionBooking: (id: string, body: TransitionBookingBody, idempotencyKey: string) =>
      request<Event>(`/api/v1/bookings/${id}/transition`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: JSON.stringify(body),
      }),

    getBooking: (id: string) => request<Booking>(`/api/v1/bookings/${id}`),

    listBookings: (params: ListBookingsParams) =>
      request<PaginatedResponse<Booking>>(
        `/api/v1/bookings${qs(params as Record<string, string | number | undefined>)}`,
      ),

    // Crypto deposit (docs/plans/2026-07-11-crypto-deposit-sweep-design.md).
    // 404s if the holder has none yet — use ensureDepositAddress to issue one.
    getDepositAddress: (holder: number) =>
      request<DepositAddress>(`/api/v1/holders/${holder}/deposit-address`),

    // Idempotent: repeated calls for the same holder always return the same
    // address.
    ensureDepositAddress: (holder: number) =>
      request<DepositAddress>(`/api/v1/holders/${holder}/deposit-address`, {
        method: "POST",
      }),

    // Deposits parked in human review (M3 compensating controls) — the
    // `review` status IS the queue. Zero ledger effect until approved.
    listDepositReviews: (params: { cursor?: string; limit?: number }) =>
      request<PaginatedResponse<Booking>>(`/api/v1/deposits/reviews${qs(params)}`),

    // Idempotent: no-op returning the current booking if already confirmed.
    approveDepositReview: (uid: string, idempotencyKey: string) =>
      request<Booking>(`/api/v1/deposits/${uid}/review/approve`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    // Idempotent: no-op returning the current booking if already failed.
    // No journal is ever posted.
    rejectDepositReview: (uid: string, reason: string, idempotencyKey: string) =>
      request<Booking>(`/api/v1/deposits/${uid}/review/reject`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
        body: JSON.stringify({ reason }),
      }),

    // Events (outbound)
    getEvent: (id: string) => request<Event>(`/api/v1/events/${id}`),

    listEvents: (params: {
      classification_code?: string;
      booking_uid?: string;
      to_status?: string;
      cursor?: string;
      limit?: number;
    }) => request<PaginatedResponse<Event>>(`/api/v1/events${qs(params)}`),

    // Classifications
    listClassifications: (activeOnly?: boolean) =>
      request<PaginatedResponse<Classification>>(
        `/api/v1/classifications${qs({ active_only: activeOnly })}`,
      ).then((d) => d.list),

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

    deactivateClassification: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/classifications/${id}/deactivate`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    // Journal Types
    listJournalTypes: (activeOnly?: boolean) =>
      request<PaginatedResponse<JournalType>>(
        `/api/v1/journal-types${qs({ active_only: activeOnly })}`,
      ).then((d) => d.list),

    createJournalType: (body: { code: string; name: string }) =>
      request<JournalType>("/api/v1/journal-types", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    deactivateJournalType: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/journal-types/${id}/deactivate`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    // Templates
    listTemplates: (activeOnly?: boolean) =>
      request<PaginatedResponse<EntryTemplate>>(
        `/api/v1/templates${qs({ active_only: activeOnly })}`,
      ).then((d) => d.list),

    createTemplate: (body: {
      code: string;
      name: string;
      journal_type_uid: string;
      lines: Array<{
        classification_uid: string;
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

    deactivateTemplate: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/templates/${id}/deactivate`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    previewTemplate: (
      code: string,
      params: { holder_id: number; currency_uid: string } & Record<
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
      request<PaginatedResponse<Currency>>(
        `/api/v1/currencies${qs({ active_only: activeOnly })}`,
      ).then((d) => d.list),

    createCurrency: (body: { code: string; name: string; exponent: number }) =>
      request<Currency>("/api/v1/currencies", {
        method: "POST",
        body: JSON.stringify(body),
      }),

    deactivateCurrency: (id: string, idempotencyKey: string) =>
      request<void>(`/api/v1/currencies/${id}/deactivate`, {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    // Reconciliation
    reconcileGlobal: (idempotencyKey: string) =>
      request<ReconcileResult>("/api/v1/reconcile", {
        method: "POST",
        headers: { "Idempotency-Key": idempotencyKey },
      }),

    reconcileAccount: (holder: number, currencyUid: string) =>
      request<ReconcileResult>("/api/v1/reconcile/account", {
        method: "POST",
        body: JSON.stringify({ holder, currency_uid: currencyUid }),
      }),

    // Snapshots
    listSnapshots: (params: {
      holder?: number;
      currency_uid?: string;
      start?: string;
      end?: string;
    }) =>
      request<PaginatedResponse<Snapshot>>(`/api/v1/snapshots${qs(params)}`).then(
        (d) => d.list,
      ),
  };
}

export type LedgerClient = ReturnType<typeof createLedgerClient>;
