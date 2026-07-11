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
  /**
   * Cache-identity of the CURRENT user (any stable per-user string — your
   * session user id). It is folded into every wallet query key, so a host
   * that reuses one QueryClient across account switches never serves one
   * holder's cached balances to another. Omit when the provider remounts
   * with a fresh QueryClient per session (the default provider behavior).
   */
  scope?: string;
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

export interface WalletDepositAddress {
  uid: string;
  /** EIP-55 checksummed EVM address. */
  address: string;
  created_at: string;
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
  // Token cache holds the PROMISE, not the resolved value: the assignment in
  // refreshToken is synchronous, so N concurrent requests (WalletPanel
  // mounts balances + transactions + holds together) share ONE getToken()
  // call instead of minting N tokens. A rejected fetch clears the cache so
  // the next request retries instead of caching the failure forever.
  let tokenPromise: Promise<string> | null = null;

  function refreshToken(): Promise<string> {
    const p = config.getToken!().catch((err: unknown) => {
      if (tokenPromise === p) tokenPromise = null;
      throw err;
    });
    tokenPromise = p;
    return p;
  }

  async function request<T>(
    path: string,
    init?: RequestInit,
    retried = false,
  ): Promise<T> {
    const fetchImpl = config.fetch ?? globalThis.fetch;
    const headers: Record<string, string> = {
      "Content-Type": "application/json",
      ...(init?.headers as Record<string, string> | undefined),
    };
    let usedToken: Promise<string> | null = null;
    if (config.getToken) {
      usedToken = tokenPromise ?? refreshToken();
      headers["Authorization"] = `Bearer ${await usedToken}`;
    }

    const res = await fetchImpl(`${config.baseUrl}${path}`, {
      ...init,
      headers,
    });

    if (res.status === 401 && config.getToken && !retried) {
      // Expired/rotated token: refresh once and retry; a second 401 falls
      // through to the error path below on the retried call. Concurrent
      // 401s dedupe too — only the first replaces the cached promise, the
      // rest see the already-refreshed one and just retry with it.
      if (tokenPromise === usedToken) {
        refreshToken();
      }
      return request<T>(path, init, true);
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
    /** Cache-identity for query keys (see WalletClientConfig.scope). */
    scope: config.scope ?? "",

    listBalances: (currencyUid?: string) =>
      request<{ list: WalletBalance[] }>(
        `/holder/balances${qs({ currency_uid: currencyUid })}`,
      ).then((d) => d.list),

    listTransactions: (params: { cursor?: string; limit?: number }) =>
      request<WalletTransactionsPage>(`/holder/transactions${qs(params)}`),

    listHolds: () =>
      request<{ list: WalletHold[] }>("/holder/holds").then((d) => d.list),

    // 404s if the token-bound holder has none yet — use ensureDepositAddress
    // to issue one. The holder is never a request parameter, only the token.
    getDepositAddress: () =>
      request<WalletDepositAddress>("/holder/deposit-address"),

    // Idempotent: repeated calls for the same holder always return the same
    // address.
    ensureDepositAddress: () =>
      request<WalletDepositAddress>("/holder/deposit-address", {
        method: "POST",
      }),
  };
}

export type WalletClient = ReturnType<typeof createWalletClient>;
