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
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-balances", () => {
  test("useBalances keys ['ledger','balances',holder]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/balances/5`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: [{ account_holder: 5, currency_uid: 1, classification_uid: 1, balance: "10" }],
        }),
      ),
    );
    const { result } = renderHook(() => useBalances(5), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
    expect(qc.getQueryCache().find({ queryKey: ["ledger", "balances", 5] })).toBeDefined();
  });

  test("useBalancesByCurrency keys ['ledger','balances',holder,currency]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/balances/5/cur-2`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: [] }),
      ),
    );
    const { result } = renderHook(() => useBalancesByCurrency(5, "cur-2"), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "balances", 5, "cur-2"] }),
    ).toBeDefined();
  });
});
