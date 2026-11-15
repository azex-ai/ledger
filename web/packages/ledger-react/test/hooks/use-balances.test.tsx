import { testCacheScope, testQueryScope } from "../cache-scope";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import { useBalances, useBalancesByCurrency } from "../../src/hooks/use-balances";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc, cacheScope: testCacheScope }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-balances", () => {
  test("useBalances keys include holder, currency and list shape", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/balances/5`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          data: { list: [{ account_holder: 5, currency_uid: 1, classification_uid: 1, balance: "10" }] },
        }),
      ),
    );
    const { result } = renderHook(() => useBalances(5, "cur-1"), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
    expect(qc.getQueryCache().find({ queryKey: ["ledger", testQueryScope, "balances", 5, "cur-1", "classifications"] })).toBeDefined();
  });

  test("useBalancesByCurrency keys ['ledger','balances',holder,currency]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/balances/5/cur-2`, () =>
        HttpResponse.json({ code: 200, message: null, data: [] }),
      ),
    );
    const { result } = renderHook(() => useBalancesByCurrency(5, "cur-2"), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", testQueryScope, "balances", 5, "cur-2"] }),
    ).toBeDefined();
  });
});

// Both URLs represent one currency, but one returns a list and the other an
// object. The keys must discriminate both the currency AND the response shape.
test("currency switching isolates booked lists from other currencies and aggregate objects", async () => {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } });
  const requested: string[] = [];
  server.use(http.get(`${BASE}/api/v1/balances/5`, ({ request }) => {
    const currency = new URL(request.url).searchParams.get("currency_uid");
    if (!currency) return HttpResponse.json({ code: 12003, message: { text: "currency_uid is required" }, data: null }, { status: 400 });
    requested.push(currency);
    return HttpResponse.json({ code: 200, data: { list: [{ account_holder: 5, currency_uid: currency, classification_uid: "wallet", balance: currency === "usd" ? "1234.56" : "0.00000001" }] } });
  }));
  qc.setQueryData(["ledger", testQueryScope, "balances", 5, "usd"], { total: "aggregate" });
  const { result, rerender } = renderHook(({ currency }) => useBalances(5, currency), { initialProps: { currency: "usd" }, wrapper: wrapperWith(qc) });
  await waitFor(() => expect(result.current.data?.[0]?.balance).toBe("1234.56"));
  rerender({ currency: "btc" });
  expect(result.current.data).toBeUndefined();
  await waitFor(() => expect(result.current.data?.[0]?.balance).toBe("0.00000001"));
  rerender({ currency: "usd" });
  expect(result.current.data?.[0]?.balance).toBe("1234.56");
  expect(requested).toEqual(["usd", "btc"]);
  expect(qc.getQueryData(["ledger", testQueryScope, "balances", 5, "usd"])).toEqual({ total: "aggregate" });
  expect(qc.getQueryData(["ledger", testQueryScope, "balances", 5, "btc", "classifications"])).toEqual([{ account_holder: 5, currency_uid: "btc", classification_uid: "wallet", balance: "0.00000001" }]);
});

test("a holder without a selected currency does not request an invalid balance URL", async () => {
  const qc = new QueryClient();
  let called = false;
  server.use(http.get(`${BASE}/api/v1/balances/5`, () => { called = true; return HttpResponse.json({ code: 200, data: { list: [] } }); }));
  const { result } = renderHook(() => useBalances(5, ""), { wrapper: wrapperWith(qc) });
  expect(result.current.fetchStatus).toBe("idle");
  expect(called).toBe(false);
});
