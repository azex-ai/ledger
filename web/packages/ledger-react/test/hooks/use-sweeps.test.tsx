import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import { useSweeps } from "../../src/hooks/use-sweeps";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-sweeps", () => {
  test("useSweeps resolves the sweep classification then lists bookings under ledger keys", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/classifications`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ uid: "cls-9", code: "sweep", name: "Sweep" }] },
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
    const { result } = renderHook(() => useSweeps(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pages.flatMap((p) => p.list)).toHaveLength(1);
    expect(
      qc.getQueryCache().find({
        queryKey: [
          "ledger",
          "bookings",
          "sweep",
          { classificationUid: "cls-9", limit: 20 },
        ],
      }),
    ).toBeDefined();
  });
});
