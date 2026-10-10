import { renderHook, waitFor } from "@testing-library/react";
import { dehydrate, hydrate, QueryClient } from "@tanstack/react-query";
import { describe, expect, test, vi } from "vitest";
import { createServerLedgerClient } from "../../src/server/client";
import * as prefetch from "../../src/server/prefetch";
import { LedgerProvider } from "../../src/provider/provider";
import { useBalances } from "../../src/hooks/use-balances";
import { useEntries, useJournals } from "../../src/hooks/use-journals";
import { useReservations } from "../../src/hooks/use-reservations";
import { useClassifications, useCurrencies, useJournalTypes, useTemplates } from "../../src/hooks/use-metadata";
import { useHealth, useSnapshots, useSystemBalances } from "../../src/hooks/use-system";

const PRIVATE = "http://ledger-private.test";
const BFF = "http://browser-bff.test";
const scope = { backend: "tenant-east", identity: "admin-session-42-permissions-3" };
const entries = { holder: 42, currency_uid: "usd" };
const reservations = { holder: 42, status: "active", limit: 20 };
const snapshots = { holder: 42, currency_uid: "usd", start: "2026-10-01", end: "2026-10-10" };

function freshClient() {
  return new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
}

function useAllPrefetched() {
  return [
    useJournals(20), useEntries(entries, 50), useBalances(42), useHealth(),
    useSystemBalances(), useReservations(reservations), useClassifications(true),
    useCurrencies(true), useJournalTypes(true), useTemplates(true), useSnapshots(snapshots),
  ];
}

describe("scoped server hydration", () => {
  test("all prefetch helpers hydrate distinct browser clients with a shared logical backend and identity", async () => {
    const serverFetch = vi.fn<typeof fetch>(async (url) => {
      const path = new URL(String(url)).pathname;
      return Response.json({ code: 200, message: null, data:
        path.endsWith("/health") ? { status: "server-healthy" } :
          { list: [{ uid: path, account_holder: 42, balance: "123" }], next_cursor: "" },
      });
    });
    const serverClient = createServerLedgerClient({
      baseUrl: PRIVATE, apiKey: "server-only-secret", fetch: serverFetch, cacheScope: scope,
    });
    const serverQC = freshClient();
    await Promise.all([
      prefetch.prefetchJournals(serverQC, serverClient, 20),
      prefetch.prefetchEntries(serverQC, serverClient, entries, 50),
      prefetch.prefetchBalances(serverQC, serverClient, 42),
      prefetch.prefetchSystemHealth(serverQC, serverClient),
      prefetch.prefetchSystemBalances(serverQC, serverClient),
      prefetch.prefetchReservations(serverQC, serverClient, reservations),
      prefetch.prefetchClassifications(serverQC, serverClient, true),
      prefetch.prefetchCurrencies(serverQC, serverClient, true),
      prefetch.prefetchJournalTypes(serverQC, serverClient, true),
      prefetch.prefetchTemplates(serverQC, serverClient, true),
      prefetch.prefetchSnapshots(serverQC, serverClient, snapshots),
    ]);
    expect(serverFetch).toHaveBeenCalledTimes(11);
    const serialized = JSON.stringify(dehydrate(serverQC));
    expect(serialized).not.toContain("server-only-secret");
    expect(serialized).not.toContain(PRIVATE);

    // Real dehydration/JSON transport/hydration, not reuse of the server cache.
    const browserQC = freshClient();
    hydrate(browserQC, JSON.parse(serialized));
    const browserFetch = vi.fn<typeof fetch>(async () => { throw new Error("unexpected hydration refetch"); });
    const { result } = renderHook(useAllPrefetched, {
      wrapper: ({ children }) => <LedgerProvider config={{
        baseUrl: BFF, fetch: browserFetch, cacheScope: { ...scope }, queryClient: browserQC,
      }}>{children}</LedgerProvider>,
    });
    expect(result.current.every((q) => q.isSuccess)).toBe(true);
    expect(result.current[3].data).toEqual({ status: "server-healthy" });
    await waitFor(() => expect(result.current.every((q) => q.isSuccess)).toBe(true));
    expect(browserFetch).not.toHaveBeenCalled();
    expect(browserQC.getQueryCache().getAll()).toHaveLength(11);
  });

  test.each(["backend", "identity", "default"] as const)(
    "hydrated data is not consumed when browser %s differs",
    async (field) => {
      const serverQC = freshClient();
      const serverClient = createServerLedgerClient({
        baseUrl: PRIVATE, cacheScope: field === "default" ? undefined : scope,
        fetch: async () => Response.json({ code: 200, message: null, data: { status: "private-A" } }),
      });
      await prefetch.prefetchSystemHealth(serverQC, serverClient);
      const browserQC = freshClient();
      hydrate(browserQC, JSON.parse(JSON.stringify(dehydrate(serverQC))));
      const browserFetch = vi.fn<typeof fetch>(async () => Response.json({
        code: 200, message: null, data: { status: "browser-B" },
      }));
      const browserScope = field === "default" ? undefined : { ...scope, [field]: "other" };
      const { result } = renderHook(useHealth, {
        wrapper: ({ children }) => <LedgerProvider config={{
          baseUrl: BFF, cacheScope: browserScope, fetch: browserFetch, queryClient: browserQC,
        }}>{children}</LedgerProvider>,
      });
      expect(result.current.data).toBeUndefined();
      await waitFor(() => expect(result.current.data?.status).toBe("browser-B"));
      expect(browserFetch).toHaveBeenCalledTimes(1);
      expect(browserQC.getQueryCache().getAll()).toHaveLength(2);
    },
  );
});
