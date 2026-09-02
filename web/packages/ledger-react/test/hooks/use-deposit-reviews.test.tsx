import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useDepositReviews,
  useApproveDepositReview,
  useRejectDepositReview,
} from "../../src/hooks/use-deposit-reviews";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-deposit-reviews", () => {
  test("useDepositReviews keys ['ledger','deposit-reviews',limit]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/deposits/reviews`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          data: { list: [{ uid: "bk-1" }, { uid: "bk-2" }], next_cursor: "" },
        }),
      ),
    );
    const { result } = renderHook(() => useDepositReviews(20), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pages.flatMap((p) => p.list)).toHaveLength(2);
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "deposit-reviews", 20] }),
    ).toBeDefined();
  });

  test("useApproveDepositReview optimistically removes uid, rolls back on error", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/deposits/reviews`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          data: { list: [{ uid: "bk-1" }, { uid: "bk-2" }], next_cursor: "" },
        }),
      ),
      // Delayed rejection: gives the assertion below a window to observe the
      // optimistic removal before the mutation settles and rolls it back.
      http.post(`${BASE}/api/v1/deposits/bk-1/review/approve`, async () => {
        await new Promise((r) => setTimeout(r, 50));
        return HttpResponse.json(
          { code: 40900, message: { text: "Conflict" }, data: null },
          { status: 409 },
        );
      }),
    );
    const { result: list } = renderHook(() => useDepositReviews(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(list.current.isSuccess).toBe(true));

    const { result: approve } = renderHook(() => useApproveDepositReview(), {
      wrapper: wrapperWith(qc),
    });
    approve.current.mutate("bk-1");

    // Optimistic removal happens synchronously with the mutation.
    await waitFor(() =>
      expect(
        list.current.data?.pages.flatMap((p) => p.list),
      ).toHaveLength(1),
    );

    await waitFor(() => expect(approve.current.isError).toBe(true));
    // Rolled back after the server rejects.
    await waitFor(() =>
      expect(
        list.current.data?.pages.flatMap((p) => p.list),
      ).toHaveLength(2),
    );
  });

  test("useRejectDepositReview requires a reason and invalidates on settle", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/deposits/reviews`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          data: { list: [{ uid: "bk-1" }], next_cursor: "" },
        }),
      ),
    );
    let capturedBody: unknown;
    server.use(
      http.post(`${BASE}/api/v1/deposits/bk-1/review/reject`, async ({ request }) => {
        capturedBody = await request.json();
        return HttpResponse.json({
          code: 200,
          message: null,
          data: { uid: "bk-1", status: "failed" },
        });
      }),
    );
    const { result } = renderHook(() => useRejectDepositReview(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate({ uid: "bk-1", reason: "amount mismatch" });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(capturedBody).toEqual({ reason: "amount mismatch" });
  });

  // J-13 (2026-09-02 web audit): this hook is page-scoped — one instance
  // backs every row's Approve button — so a failed attempt on one uid must
  // not hand its idempotency key to a later attempt on a DIFFERENT uid
  // (the server's three-state semantics would then permanently fail the
  // second uid: same key + different payload -> ErrConflict).
  test("a failed approve on one uid does not poison the key for a different uid", async () => {
    const qc = new QueryClient();
    const keysByUid: Record<string, string[]> = { "bk-1": [], "bk-2": [] };
    server.use(
      http.post(`${BASE}/api/v1/deposits/bk-1/review/approve`, ({ request }) => {
        keysByUid["bk-1"].push(request.headers.get("idempotency-key") ?? "");
        return HttpResponse.json(
          { code: 40900, message: { text: "Conflict" }, data: null },
          { status: 409 },
        );
      }),
      http.post(`${BASE}/api/v1/deposits/bk-2/review/approve`, ({ request }) => {
        keysByUid["bk-2"].push(request.headers.get("idempotency-key") ?? "");
        return HttpResponse.json({ code: 200, message: null, data: { uid: "bk-2", status: "confirmed" } });
      }),
    );
    const { result } = renderHook(() => useApproveDepositReview(), {
      wrapper: wrapperWith(qc),
    });

    result.current.mutate("bk-1");
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(keysByUid["bk-1"]).toHaveLength(1);

    result.current.mutate("bk-2");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(keysByUid["bk-2"]).toHaveLength(1);
    expect(keysByUid["bk-2"][0]).not.toBe(keysByUid["bk-1"][0]);
  });
});
