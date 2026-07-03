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
        HttpResponse.json({ code: 200, message: "ok", data: { status: "ok" } }),
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
        HttpResponse.json({ code: 200, message: "ok", data: [] }),
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
        HttpResponse.json({ code: 200, message: "ok", data: [] }),
      ),
    );
    const params = { holder: 9 };
    const { result } = renderHook(() => useSnapshots(params), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "snapshots", params] }),
    ).toBeDefined();
  });
});
