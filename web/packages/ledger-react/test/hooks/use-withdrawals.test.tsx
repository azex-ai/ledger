import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useWithdrawals,
  useReserveWithdraw,
} from "../../src/hooks/use-withdrawals";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-withdrawals", () => {
  test("useWithdrawals lists bookings under ledger keys", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/classifications`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: [{ uid: "cls-4", code: "withdraw", name: "Withdraw" }],
        }),
      ),
      http.get(`${BASE}/api/v1/bookings`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ uid: "bk-2" }], next_cursor: "" },
        }),
      ),
    );
    const params = { holder: 7 };
    const { result } = renderHook(() => useWithdrawals(params), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
    expect(
      qc.getQueryCache().find({
        queryKey: ["ledger", "bookings", "withdraw", { ...params, classificationUid: "cls-4" }],
      }),
    ).toBeDefined();
  });

  test("useReserveWithdraw invalidates ledger bookings", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/bookings/uid-2/transition`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { id: 10 } }),
      ),
    );
    const { result } = renderHook(() => useReserveWithdraw(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate("uid-2");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "bookings"]);
  });
});
