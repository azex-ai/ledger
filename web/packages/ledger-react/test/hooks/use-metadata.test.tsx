import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useClassifications,
  useCurrencies,
  useDeactivateCurrency,
} from "../../src/hooks/use-metadata";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-metadata", () => {
  test("useClassifications keys ['ledger','classifications',activeOnly]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/classifications`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: [] }),
      ),
    );
    const { result } = renderHook(() => useClassifications(true), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "classifications", true] }),
    ).toBeDefined();
  });

  test("useCurrencies passes activeOnly and keys ['ledger','currencies',activeOnly]", async () => {
    const qc = new QueryClient();
    let receivedActiveOnly: string | null = null;
    server.use(
      http.get(`${BASE}/api/v1/currencies`, ({ request }) => {
        receivedActiveOnly = new URL(request.url).searchParams.get("active_only");
        return HttpResponse.json({ code: 200, message: "ok", data: [] });
      }),
    );
    const { result } = renderHook(() => useCurrencies(true), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(receivedActiveOnly).toBe("true");
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "currencies", true] }),
    ).toBeDefined();
  });

  test("useDeactivateCurrency invalidates ledger currencies", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/currencies/3/deactivate`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: null }),
      ),
    );
    const { result } = renderHook(() => useDeactivateCurrency(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate(3);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "currencies"]);
  });
});
