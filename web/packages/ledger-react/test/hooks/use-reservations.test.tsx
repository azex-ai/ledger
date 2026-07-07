import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useReservations,
  useReleaseReservation,
} from "../../src/hooks/use-reservations";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-reservations", () => {
  test("useReservations keys ['ledger','reservations',params]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/reservations`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { list: [{ id: 1 }] } }),
      ),
    );
    const params = { holder: 5 };
    const { result } = renderHook(() => useReservations(params), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pages.flatMap((p) => p.list)).toHaveLength(1);
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "reservations", params] }),
    ).toBeDefined();
  });

  test("useReleaseReservation invalidates ledger reservations", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/reservations/uid-1/release`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: null }),
      ),
    );
    const { result } = renderHook(() => useReleaseReservation(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate("uid-1");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "reservations"]);
  });
});
