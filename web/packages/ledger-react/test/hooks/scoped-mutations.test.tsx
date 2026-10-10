import { act, renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, test, vi } from "vitest";
import { createLedgerClient, type LedgerClient } from "../../src/client/client";
import { LedgerClientContext } from "../../src/provider/context";
import { usePostJournal, useReverseJournal } from "../../src/hooks/use-journals";
import { useCreateCurrency } from "../../src/hooks/use-metadata";
import { useReleaseReservation } from "../../src/hooks/use-reservations";
import { useApproveDepositReview, useDepositReviews, useRejectDepositReview } from "../../src/hooks/use-deposit-reviews";
import { prefetchBalances, prefetchCurrencies, prefetchJournals, prefetchSystemBalances } from "../../src/server/prefetch";

const BASE = "http://ledger.test";
const journal = {
  journal_type_uid: "type-1", idempotency_key: "attempt-1",
  entries: [
    { account_holder: 42, currency_uid: "usd", classification_uid: "available", entry_type: "credit" as const, amount: "1" },
    { account_holder: -1, currency_uid: "usd", classification_uid: "system", entry_type: "debit" as const, amount: "1" },
  ],
};

function response(data: unknown) {
  return Response.json({ code: 200, message: null, data });
}

function freshClient() {
  return new QueryClient({ defaultOptions: { queries: { staleTime: Infinity, retry: false } } });
}

function deferred<T>() {
  let resolve: (value: T) => void = () => { throw new Error("not initialized"); };
  const promise = new Promise<T>((done) => { resolve = done; });
  return { promise, resolve };
}

function clientFor(identity: string, fetcher: typeof fetch) {
  return createLedgerClient({ baseUrl: BASE, fetch: fetcher, cacheScope: { backend: "east", identity } });
}

async function prefetchAffected(qc: QueryClient, client: LedgerClient) {
  await Promise.all([
    prefetchBalances(qc, client, 42), prefetchSystemBalances(qc, client),
    prefetchJournals(qc, client), prefetchCurrencies(qc, client),
  ]);
}

function scopedQueries(qc: QueryClient, client: LedgerClient) {
  return qc.getQueryCache().getAll().filter((q) =>
    JSON.stringify(q.queryKey[1]) === JSON.stringify(client.cacheScope));
}

const actions = [
  {
    name: "generic journal mutation", affected: ["journals", "balances", "system-balances"],
    useAction: function useAction() {
      const mutation = usePostJournal();
      return { mutation, run: () => mutation.mutateAsync(journal) };
    },
  },
  {
    name: "direct reversal mutation", affected: ["journals", "balances", "system-balances"],
    useAction: function useAction() {
      const mutation = useReverseJournal();
      return { mutation, run: () => mutation.mutateAsync({ id: "journal-1", reason: "correction" }) };
    },
  },
  {
    name: "metadata mutation", affected: ["currencies"],
    useAction: function useAction() {
      const mutation = useCreateCurrency();
      return { mutation, run: () => mutation.mutateAsync({ code: "USD", name: "US dollar", exponent: 2 }) };
    },
  },
];

describe("mutation cache boundaries", () => {
  test.each(["same identity", "new identity"] as const)("failed-attempt idempotency keys respect %s on reconfiguration", async (boundary) => {
    const qc = freshClient();
    const observedKeys: string[] = [];
    let fail = true;
    const fetcher: typeof fetch = async (_url, init) => {
      observedKeys.push(new Headers(init?.headers).get("idempotency-key") ?? "");
      return fail
        ? Response.json({ code: 40900, message: { text: "retry later" }, data: null }, { status: 409 })
        : new Response(null, { status: 204 });
    };
    let activeClient = createLedgerClient({
      baseUrl: BASE, fetch: fetcher, apiKey: "secret-a",
      cacheScope: { backend: "east", identity: "session-A" },
    });
    const { result, rerender } = renderHook(useReleaseReservation, {
      wrapper: ({ children }) => <QueryClientProvider client={qc}>
        <LedgerClientContext.Provider value={activeClient}>{children}</LedgerClientContext.Provider>
      </QueryClientProvider>,
    });
    await act(async () => { await expect(result.current.mutateAsync("reservation-1")).rejects.toThrow("retry later"); });
    activeClient = createLedgerClient({
      baseUrl: BASE, fetch: fetcher, apiKey: "secret-b",
      cacheScope: { backend: "east", identity: boundary === "same identity" ? "session-A" : "session-B" },
    });
    rerender();
    fail = false;
    await act(async () => { await result.current.mutateAsync("reservation-1"); });
    expect(observedKeys).toHaveLength(2);
    expect(observedKeys[0]).not.toBe("");
    expect(observedKeys[0] === observedKeys[1]).toBe(boundary === "same identity");
  });

  test.each(actions)("pending $name keeps the original callbacks after identity switches", async ({ useAction, affected }) => {
    const qc = freshClient();
    const lateA = deferred<Response>();
    const fetchA = vi.fn<typeof fetch>(async (_url, init) =>
      init?.method === "POST" ? lateA.promise : response({ list: [], next_cursor: "" }));
    const fetchB = vi.fn<typeof fetch>(async (_url, init) =>
      init?.method === "POST" ? response({ uid: "receipt-B" }) : response({ list: [], next_cursor: "" }));
    const clientA = clientFor("session-A", fetchA);
    const clientB = clientFor("session-B", fetchB);
    await Promise.all([prefetchAffected(qc, clientA), prefetchAffected(qc, clientB)]);

    // Deliberately keep the hook mounted via the context, so this exercises
    // TanStack mutationKey reset, not only LedgerProvider's keyed remount.
    let activeClient = clientA;
    const { result, rerender } = renderHook(() => useAction(), {
      wrapper: ({ children }) => <QueryClientProvider client={qc}>
        <LedgerClientContext.Provider value={activeClient}>{children}</LedgerClientContext.Provider>
      </QueryClientProvider>,
    });
    let receipt: Promise<unknown> = Promise.resolve();
    act(() => { receipt = result.current.run(); });
    await waitFor(() => expect(result.current.mutation.isPending).toBe(true));
    await waitFor(() => expect(fetchA.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(1));
    activeClient = clientB;
    rerender();
    await act(async () => {
      lateA.resolve(response({ uid: "receipt-A" }));
      await receipt;
    });

    expect(scopedQueries(qc, clientA)).toHaveLength(4);
    for (const query of scopedQueries(qc, clientA)) {
      expect(query.state.isInvalidated, "completed request must invalidate its original scope")
        .toBe(affected.includes(String(query.queryKey[2])));
    }
    expect(scopedQueries(qc, clientB)).toHaveLength(4);
    expect(scopedQueries(qc, clientB).every((q) => !q.state.isInvalidated)).toBe(true);
    expect(fetchB.mock.calls.filter(([, init]) => init?.method === "POST")).toHaveLength(0);
    expect(result.current.mutation.data).toBeUndefined();
    expect(result.current.mutation.isIdle).toBe(true);
  });

  test.each(["approve", "reject"] as const)("%s optimistic writes, rollback and invalidation stay in the originating scope", async (operation) => {
    const qc = freshClient();
    const rejected = deferred<Response>();
    const queue = { list: [{ uid: "bk-1", account_holder: 42 }, { uid: "bk-2", account_holder: 42 }], next_cursor: "" };
    const fetchA = vi.fn<typeof fetch>(async (_url, init) => init?.method === "POST" ? rejected.promise : response(queue));
    const fetchB = vi.fn<typeof fetch>(async () => response(queue));
    const clientA = clientFor("session-A", fetchA);
    const clientB = clientFor("session-B", fetchB);
    let activeClient = clientA;
    const wrapper = ({ children }: { children: React.ReactNode }) => <QueryClientProvider client={qc}>
      <LedgerClientContext.Provider value={activeClient}>{children}</LedgerClientContext.Provider>
    </QueryClientProvider>;
    const { result, rerender } = renderHook(() => ({
      queue: useDepositReviews(), approve: useApproveDepositReview(), reject: useRejectDepositReview(),
    }), { wrapper });
    await waitFor(() => expect(result.current.queue.data?.pages[0].list).toHaveLength(2));
    let receipt: Promise<unknown> = Promise.resolve();
    act(() => {
      receipt = (operation === "approve"
        ? result.current.approve.mutateAsync("bk-1")
        : result.current.reject.mutateAsync({ uid: "bk-1", reason: "mismatch" })).catch((err: unknown) => err);
    });
    await waitFor(() => expect(result.current.queue.data?.pages[0].list).toHaveLength(1));
    activeClient = clientB;
    rerender();
    expect(result.current.queue.data).toBeUndefined();
    await waitFor(() => expect(result.current.queue.data?.pages[0].list).toHaveLength(2));
    await act(async () => {
      rejected.resolve(Response.json({ code: 40900, message: { text: "Conflict" }, data: null }, { status: 409 }));
      await receipt;
    });

    expect(result.current.queue.data?.pages[0].list).toHaveLength(2);
    expect(fetchB).toHaveBeenCalledTimes(1);
    const originalQueue = scopedQueries(qc, clientA)[0];
    expect(originalQueue.state.data).toEqual({ pages: [queue], pageParams: [undefined] });
    expect(originalQueue.state.isInvalidated).toBe(true);
    expect(scopedQueries(qc, clientB)[0].state.isInvalidated).toBe(false);
  });
});
