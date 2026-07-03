import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import { useDeposits, useConfirmDeposit } from "../../src/hooks/use-deposits";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-deposits", () => {
  test("useDeposits resolves classification then lists bookings under ledger keys", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/classifications`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: [{ uid: "cls-3", code: "deposit", name: "Deposit" }],
        }),
      ),
      http.get(`${BASE}/api/v1/bookings`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ uid: "bk-1" }], next_cursor: "" },
        }),
      ),
    );
    const params = { holder: 5 };
    const { result } = renderHook(() => useDeposits(params), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "classifications", true] }),
    ).toBeDefined();
    expect(
      qc.getQueryCache().find({
        queryKey: ["ledger", "bookings", "deposit", { ...params, classificationUid: "cls-3" }],
      }),
    ).toBeDefined();
  });

  test("useConfirmDeposit invalidates ledger bookings + balances", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/bookings/uid-1/transition`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { id: 99 } }),
      ),
    );
    const { result } = renderHook(() => useConfirmDeposit(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate({ id: "uid-1", actual_amount: "10", channel_ref: "tx" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "bookings"]);
    expect(keys).toContainEqual(["ledger", "balances"]);
  });
});
