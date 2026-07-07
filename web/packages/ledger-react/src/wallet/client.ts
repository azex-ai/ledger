import { ApiRequestError } from "../client/client";

/*
 * Wallet client — the end-user consumption surface of the holder-scoped
 * wallet API (`/api/v1/holder/*`). A separate client from the admin
 * LedgerClient on purpose: different trust domain (holder token / host BFF
 * session, never an API key) and a read-only method set.
 *
 * Auth is callback-injected (docs/plans/2026-07-08-holder-scoped-wallet-surface.md §3.6):
 *  - Topology A (token): pass `getToken` — it is called for the first
 *    request and again ONCE on a 401 (expired token) before the request is
 *    retried; a second 401 surfaces as ApiRequestError.
 *  - Topology B (BFF): omit `getToken` — requests ride the host's own
 *    same-origin session (cookies), the BFF injects the holder.
 */

export interface WalletClientConfig {
  /** Base URL up to (not including) `/holder/*` — e.g. "/api/v1". */
  baseUrl: string;
  /**
   * Returns a holder token minted by the host backend. Called lazily for
   * the first request and once more per request after a 401. MUST be a
   * STABLE reference (module-level or `useCallback`'d) — WalletProvider
   * keys its client memo on it.
   */
  getToken?: () => Promise<string>;
  /** Optional fetch override (server use / tests). Stable reference, see above. */
  fetch?: typeof fetch;
}

// ---- Wire types (snake_case end-to-end, amounts as strings) ----

export interface WalletBalance {
  currency_uid: string;
  currency_code: string;
  available: string;
  pending: string;
  locked: string;
  /** = available + pending + locked */
  total: string;
}

export interface WalletTransaction {
  /** Journal uid — trace anchor, safe to display. */
  uid: string;
  /** Stable code — the anchor for product-side label overrides / i18n. */
  kind: string;
  /** Library-side default display label. */
  kind_label: string;
  direction: "in" | "out";
  amount: string;
  currency_uid: string;
  currency_code: string;
  occurred_at: string;
  /** Non-empty when this row reverses that journal (refund marker). */
  reversal_of_uid: string;
  memo: string;
}

export interface WalletHold {
  uid: string;
  amount: string;
  currency_uid: string;
  currency_code: string;
  created_at: string;
  expires_at: string;
}

export interface WalletTransactionsPage {
  list: WalletTransaction[];
  next_cursor: string;
}

interface Envelope<T> {
  code: number;
  message: string;
  data: T;
}

function qs(params: Record<string, string | number | undefined>): string {
  const entries = Object.entries(params).filter(
    ([, v]) => v !== undefined && v !== "",
  );
  if (entries.length === 0) return "";
  return (
    "?" +
    new URLSearchParams(entries.map(([k, v]) => [k, String(v)])).toString()
  );
}

export function createWalletClient(config: WalletClientConfig) {
  // Token cache: shared across requests, replaced on 401-refresh.
  let token: string | null = null;

  async function request<T>(path: string, retried = false): Promise<T> {
    const fetchImpl = config.fetch ?? globalThis.fetch;
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
    };
    if (config.getToken) {
      token ??= await config.getToken();
      headers["Authorization"] = `Bearer ${token}`;
    }

    const res = await fetchImpl(`${config.baseUrl}${path}`, { headers });

    if (res.status === 401 && config.getToken && !retried) {
      // Expired/rotated token: refresh once and retry; a second 401 falls
      // through to the error path below on the retried call.
      token = await config.getToken();
      return request<T>(path, true);
    }

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

    const envelope: Envelope<T> = await res.json();
    return envelope.data;
  }

  return {
    listBalances: (currencyUid?: string) =>
      request<{ list: WalletBalance[] }>(
        `/holder/balances${qs({ currency_uid: currencyUid })}`,
      ).then((d) => d.list),

    listTransactions: (params: { cursor?: string; limit?: number }) =>
      request<WalletTransactionsPage>(`/holder/transactions${qs(params)}`),

    listHolds: () =>
      request<{ list: WalletHold[] }>("/holder/holds").then((d) => d.list),
  };
}

export type WalletClient = ReturnType<typeof createWalletClient>;
