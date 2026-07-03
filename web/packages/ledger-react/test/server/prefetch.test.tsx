import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { createServerLedgerClient } from "../../src/server/client";
import {
  prefetchJournals,
  prefetchBalances,
  prefetchSystemHealth,
} from "../../src/server/prefetch";
import { ledgerKeys } from "../../src/hooks/keys";
import { LedgerClientContext } from "../../src/provider/context";
import { useJournals } from "../../src/hooks/use-journals";
import { useBalances } from "../../src/hooks/use-balances";
import { useHealth } from "../../src/hooks/use-system";
import { server } from "../setup";

const BASE = "http://ledger.test";

// A fresh-data QueryClient mirrors the standard RSC hydration setup: hosts set
// a non-zero `staleTime` so server-prefetched data is considered fresh and the
// client does NOT background-revalidate on mount. With the default
// `staleTime: 0`, React Query still hydrates instantly (data present on first
// render) but fires a background refetch — that refetch is expected RQ
// behavior, orthogonal to whether the prefetch key matched. Pinning staleTime
// lets the call-count assertion isolate "did the key match" (1 fetch total =
// only the server prefetch) from incidental revalidation.
function freshClient() {
  return new QueryClient({
    defaultOptions: { queries: { staleTime: 60_000 } },
  });
}

// Wrap the hook in BOTH the prefetched QueryClient and a LedgerClientContext
// so the client hook resolves the same client/cache. We provide the context
// directly (rather than <LedgerProvider>) to reuse the exact `qc` the server
// prefetched into — proving the key matches.
function wrapper(qc: QueryClient, client: ReturnType<typeof createServerLedgerClient>) {
  return ({ children }: { children: ReactNode }) => (
    <QueryClientProvider client={qc}>
      <LedgerClientContext.Provider value={client}>
        {children}
      </LedgerClientContext.Provider>
    </QueryClientProvider>
  );
}

describe("server prefetch round-trip", () => {
  test("prefetchJournals seeds infinite-query cache; useJournals hits it with no extra fetch", async () => {
    let calls = 0;
    server.use(
      http.get(`${BASE}/api/v1/journals`, () => {
        calls += 1;
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ id: 1 }, { id: 2 }], next_cursor: "" },
        });
      }),
    );

    const client = createServerLedgerClient({ baseUrl: BASE });
    const qc = freshClient();

    // Server-side prefetch.
    await prefetchJournals(qc, client, 20);

    // Cache is populated under the SHARED key, in useInfiniteQuery shape.
    const cached = qc.getQueryData(ledgerKeys.journals(20)) as
      | { pages: Array<{ list: Array<{ id: number }> }> }
      | undefined;
    expect(cached?.pages[0].list).toEqual([{ id: 1 }, { id: 2 }]);
    expect(calls).toBe(1);

    // Client hook hydrates immediately — success on first render, no refetch.
    const { result } = renderHook(() => useJournals(20), {
      wrapper: wrapper(qc, client),
    });
    expect(result.current.isSuccess).toBe(true);
    expect(result.current.data?.pages[0].list).toEqual([{ id: 1 }, { id: 2 }]);

    // Give any (unwanted) background fetch a chance to fire, then assert none did.
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(calls).toBe(1);
  });

  test("prefetchBalances seeds cache; useBalances hits it with no extra fetch", async () => {
    let calls = 0;
    server.use(
      http.get(`${BASE}/api/v1/balances/42`, () => {
        calls += 1;
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: [{ currency_uid: 1, balance: "100" }],
        });
      }),
    );

    const client = createServerLedgerClient({ baseUrl: BASE });
    const qc = freshClient();

    await prefetchBalances(qc, client, 42);

    expect(qc.getQueryData(ledgerKeys.balances(42))).toEqual([
      { currency_uid: 1, balance: "100" },
    ]);
    expect(calls).toBe(1);

    const { result } = renderHook(() => useBalances(42), {
      wrapper: wrapper(qc, client),
    });
    expect(result.current.isSuccess).toBe(true);
    expect(result.current.data).toEqual([{ currency_uid: 1, balance: "100" }]);

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(calls).toBe(1);
  });

  test("prefetchSystemHealth seeds cache; useHealth hits it with no extra fetch", async () => {
    let calls = 0;
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () => {
        calls += 1;
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { status: "healthy" },
        });
      }),
    );

    const client = createServerLedgerClient({ baseUrl: BASE });
    const qc = freshClient();

    await prefetchSystemHealth(qc, client);

    expect(qc.getQueryData(ledgerKeys.health())).toEqual({ status: "healthy" });
    expect(calls).toBe(1);

    const { result } = renderHook(() => useHealth(), {
      wrapper: wrapper(qc, client),
    });
    expect(result.current.isSuccess).toBe(true);
    expect(result.current.data).toEqual({ status: "healthy" });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(calls).toBe(1);
  });
});
