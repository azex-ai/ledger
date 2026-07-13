import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useDepositAddress,
  useEnsureDepositAddress,
} from "../../src/hooks/use-deposit-address";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-deposit-address", () => {
  test("useDepositAddress keys ['ledger','deposit-address',holder]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/holders/7/deposit-address`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { uid: "da-1", account_holder: 7, address: "0xabc" },
        }),
      ),
    );
    const { result } = renderHook(() => useDepositAddress(7), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.address).toBe("0xabc");
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "deposit-address", 7] }),
    ).toBeDefined();
  });

  test("useDepositAddress does not retry on 404", async () => {
    const qc = new QueryClient();
    let hits = 0;
    server.use(
      http.get(`${BASE}/api/v1/holders/9/deposit-address`, () => {
        hits += 1;
        return HttpResponse.json(
          { code: 40400, message: "not found" },
          { status: 404 },
        );
      }),
    );
    const { result } = renderHook(() => useDepositAddress(9), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(hits).toBe(1);
  });

  test("useEnsureDepositAddress invalidates deposit-address", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/holders/7/deposit-address`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { uid: "da-1", account_holder: 7, address: "0xabc" },
        }),
      ),
    );
    const { result } = renderHook(() => useEnsureDepositAddress(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate(7);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "deposit-address"]);
  });
});
