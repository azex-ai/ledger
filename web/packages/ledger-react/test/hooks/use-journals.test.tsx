import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import {
  useJournals,
  useJournal,
  usePostJournal,
  useEntries,
} from "../../src/hooks/use-journals";
import { server } from "../setup";

const BASE = "http://ledger.test";

function wrapperWith(qc?: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </LedgerProvider>
  );
}

describe("use-journals", () => {
  test("useJournals loads a page and keys ['ledger','journals',limit]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/journals`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ id: 1 }], next_cursor: "" },
        }),
      ),
    );
    const { result } = renderHook(() => useJournals(20), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pages[0].list).toHaveLength(1);
    expect(qc.getQueryCache().find({ queryKey: ["ledger", "journals", 20] })).toBeDefined();
  });

  test("useJournal keys ['ledger','journal',id]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/journals/uid-7`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { journal: { uid: "uid-7" }, entries: [] },
        }),
      ),
    );
    const { result } = renderHook(() => useJournal("uid-7"), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(qc.getQueryCache().find({ queryKey: ["ledger", "journal", "uid-7"] })).toBeDefined();
  });

  test("useEntries keys ['ledger','entries',params]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/api/v1/entries`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [], next_cursor: "" },
        }),
      ),
    );
    const params = { holder: 42 };
    const { result } = renderHook(() => useEntries(params), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger", "entries", params] }),
    ).toBeDefined();
  });

  test("usePostJournal invalidates ledger keys", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");
    server.use(
      http.post(`${BASE}/api/v1/journals`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { id: 9 } }),
      ),
    );
    const { result } = renderHook(() => usePostJournal(), {
      wrapper: wrapperWith(qc),
    });
    result.current.mutate({
      journal_type_uid: "uid-1",
      idempotency_key: "k",
      entries: [],
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(keys).toContainEqual(["ledger", "journals"]);
    expect(keys).toContainEqual(["ledger", "balances"]);
  });

  test("useJournals threads cursor through fetchNextPage and stops on empty next_cursor", async () => {
    const qc = new QueryClient();
    const seenCursors: (string | null)[] = [];
    server.use(
      http.get(`${BASE}/api/v1/journals`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        seenCursors.push(cursor);
        if (!cursor) {
          // page 0
          return HttpResponse.json({
            code: 200,
            message: "ok",
            data: { list: [{ id: 1 }], next_cursor: "c1" },
          });
        }
        // page 1 (cursor=c1) — last page, empty next_cursor stops pagination
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [{ id: 2 }], next_cursor: "" },
        });
      }),
    );

    const { result } = renderHook(() => useJournals(20), {
      wrapper: wrapperWith(qc),
    });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.pages).toHaveLength(1);
    expect(result.current.hasNextPage).toBe(true);

    await result.current.fetchNextPage();

    await waitFor(() => expect(result.current.data?.pages).toHaveLength(2));
    // Page 0 uses initialPageParam "" which the client's qs() drops (no
    // cursor param → null); page 1 carries the cursor from page 0's next_cursor.
    expect(seenCursors).toEqual([null, "c1"]);
    // Both pages' data are present (appended, not replaced).
    expect(result.current.data?.pages.flatMap((p) => p.list)).toEqual([
      { id: 1 },
      { id: 2 },
    ]);
    // next_cursor "" → getNextPageParam returns undefined → no more pages.
    await waitFor(() => expect(result.current.hasNextPage).toBe(false));
  });
});
