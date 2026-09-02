import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useHealth,
  useSystemBalances,
  useSnapshots,
} from "../../src/hooks/use-system";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-system", () => {
  test("useHealth keys ['ledger','health']", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () =>
        HttpResponse.json({ code: 200, message: null, data: { status: "ok" } }),
      ),
    );
    const { result } = renderHook(() => useHealth(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.status).toBe("ok");
    expect(qc.getQueryCache().find({ queryKey: ["ledger", "health"] })).toBeDefined();
  });

  test("useSystemBalances keys ['ledger','system-balances']", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/system/balances`, () =>
        HttpResponse.json({ code: 200, message: null, data: { list: [] } }),
      ),
    );
    const { result } = renderHook(() => useSystemBalances(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "system-balances"] }),
    ).toBeDefined();
  });

  test("useSnapshots keys ['ledger','snapshots',params]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/snapshots`, () =>
        HttpResponse.json({ code: 200, message: null, data: { list: [] } }),
      ),
    );
    const params = { holder: 9, currency_uid: "cur-1", start: "2026-01-01", end: "2026-01-31" };
    const { result } = renderHook(() => useSnapshots(params), {
      wrapper: wrapperWith(qc),
    });
    expect(result.current.isDisabled).toBe(false);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "snapshots", params] }),
    ).toBeDefined();
  });

  // J-1 / J-18 (2026-09-02 web audit): currency_uid/start/end are just as
  // hard-required server-side as holder (server/handler_system.go 400s on
  // any one missing) — the query must stay disabled, not fire a
  // guaranteed-400 request, when any one of them is absent.
  test("useSnapshots stays disabled (no request) when currency_uid is missing", async () => {
    const qc = new QueryClient();
    let requested = false;
    server.use(
      http.get(`${BASE}/api/v1/snapshots`, () => {
        requested = true;
        return HttpResponse.json({ code: 200, message: null, data: { list: [] } });
      }),
    );
    const { result } = renderHook(
      () => useSnapshots({ holder: 9, start: "2026-01-01", end: "2026-01-31" }),
      { wrapper: wrapperWith(qc) },
    );
    expect(result.current.isDisabled).toBe(true);
    expect(result.current.isLoading).toBe(false);
    expect(result.current.isError).toBe(false);
    expect(result.current.data).toBeUndefined();
    expect(requested).toBe(false);
  });
});
