import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { createWalletClient } from "../../src/wallet/client";
import { WalletProvider } from "../../src/wallet/provider";
import {
  useWalletBalance,
  useWalletHolds,
  useWalletTransactions,
} from "../../src/wallet/hooks";
import { server } from "../setup";

const BASE = "http://wallet.test/api/v1";

const BALANCE = {
  currency_uid: "cur-1",
  currency_code: "USD",
  available: "75",
  pending: "0",
  locked: "25",
  total: "100",
};

describe("wallet client auth", () => {
  test("getToken is called lazily and the token rides Authorization", async () => {
    let seenAuth = "";
    server.use(
      http.get(`${BASE}/holder/balances`, ({ request }) => {
        seenAuth = request.headers.get("authorization") ?? "";
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [BALANCE] },
        });
      }),
    );
    const getToken = vi.fn(async () => "lht_tok1");
    const client = createWalletClient({ baseUrl: BASE, getToken });
    expect(getToken).not.toHaveBeenCalled();

    const list = await client.listBalances();
    expect(list).toHaveLength(1);
    expect(seenAuth).toBe("Bearer lht_tok1");
    expect(getToken).toHaveBeenCalledTimes(1);

    // Cached across requests — not re-fetched per call.
    await client.listBalances();
    expect(getToken).toHaveBeenCalledTimes(1);
  });

  test("401 refreshes the token once and retries; second 401 surfaces", async () => {
    const tokens = ["expired", "fresh"];
    const getToken = vi.fn(async () => tokens.shift() ?? "none-left");
    let calls = 0;
    server.use(
      http.get(`${BASE}/holder/holds`, ({ request }) => {
        calls++;
        if (request.headers.get("authorization") === "Bearer fresh") {
          return HttpResponse.json({
            code: 200,
            message: "ok",
            data: { list: [] },
          });
        }
        return HttpResponse.json(
          { code: 10101, message: "invalid holder token", data: null },
          { status: 401 },
        );
      }),
    );
    const client = createWalletClient({ baseUrl: BASE, getToken });

    const holds = await client.listHolds();
    expect(holds).toEqual([]);
    expect(calls).toBe(2); // original + one retry
    expect(getToken).toHaveBeenCalledTimes(2); // lazy initial + refresh

    // Now the cached token is "fresh" and keeps working; force it stale by
    // making the endpoint reject everything: exactly one refresh attempt.
    server.use(
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json(
          { code: 10101, message: "invalid holder token", data: null },
          { status: 401 },
        ),
      ),
    );
    await expect(client.listHolds()).rejects.toMatchObject({
      apiError: { code: 10101 },
    });
  });

  test("concurrent requests share ONE getToken call (promise-cached)", async () => {
    let minted = 0;
    const getToken = vi.fn(async () => {
      minted++;
      await new Promise((r) => setTimeout(r, 20)); // keep it in-flight
      return `lht_tok${minted}`;
    });
    const ok = () =>
      HttpResponse.json({ code: 200, message: "ok", data: { list: [] } });
    server.use(
      http.get(`${BASE}/holder/balances`, ok),
      http.get(`${BASE}/holder/holds`, ok),
      http.get(`${BASE}/holder/transactions`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [], next_cursor: "" },
        }),
      ),
    );
    const client = createWalletClient({ baseUrl: BASE, getToken });

    // WalletPanel's real mount pattern: three requests in parallel.
    await Promise.all([
      client.listBalances(),
      client.listHolds(),
      client.listTransactions({}),
    ]);
    expect(getToken).toHaveBeenCalledTimes(1);
  });

  test("a failed getToken is not cached — the next request retries", async () => {
    const getToken = vi
      .fn<() => Promise<string>>()
      .mockRejectedValueOnce(new Error("mint endpoint down"))
      .mockResolvedValue("lht_ok");
    server.use(
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { list: [] } }),
      ),
    );
    const client = createWalletClient({ baseUrl: BASE, getToken });
    await expect(client.listHolds()).rejects.toThrow("mint endpoint down");
    await expect(client.listHolds()).resolves.toEqual([]);
    expect(getToken).toHaveBeenCalledTimes(2);
  });

  test("omitting getToken sends no Authorization (BFF topology)", async () => {
    let seenAuth: string | null = "sentinel";
    server.use(
      http.get(`${BASE}/holder/balances`, ({ request }) => {
        seenAuth = request.headers.get("authorization");
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [] },
        });
      }),
    );
    const client = createWalletClient({ baseUrl: BASE });
    await client.listBalances();
    expect(seenAuth).toBeNull();
  });
});

function wrapperWith(qc: QueryClient) {
  return ({ children }: { children: ReactNode }) => (
    <WalletProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </WalletProvider>
  );
}

describe("wallet hooks", () => {
  test("useWalletBalance keys ['ledger-wallet','balances',currency]", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/holder/balances`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: { list: [BALANCE] },
        }),
      ),
    );
    const { result } = renderHook(() => useWalletBalance(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toHaveLength(1);
    expect(result.current.data?.[0].total).toBe("100");
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger-wallet", "", "balances", ""] }),
    ).toBeDefined();
  });

  test("useWalletTransactions pages on next_cursor", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/holder/transactions`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: {
            list: [
              {
                uid: cursor ? "j-2" : "j-1",
                kind: "deposit_confirm",
                kind_label: "Deposit",
                direction: "in",
                amount: "10",
                currency_uid: "cur-1",
                currency_code: "USD",
                occurred_at: "2026-07-08T02:00:00Z",
                reversal_of_uid: "",
                memo: "",
              },
            ],
            next_cursor: cursor ? "" : "42",
          },
        });
      }),
    );
    const { result } = renderHook(() => useWalletTransactions(1), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.hasNextPage).toBe(true);

    await result.current.fetchNextPage();
    await waitFor(() => expect(result.current.data?.pages).toHaveLength(2));
    const flat = result.current.data?.pages.flatMap((p) => p.list) ?? [];
    expect(flat.map((t) => t.uid)).toEqual(["j-1", "j-2"]);
    expect(result.current.hasNextPage).toBe(false);
  });

  test("scope isolates cache keys across account switches", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/holder/balances`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { list: [BALANCE] } }),
      ),
    );
    const wrapper = ({ children }: { children: ReactNode }) => (
      <WalletProvider config={{ baseUrl: BASE, queryClient: qc, scope: "user-A" }}>
        {children}
      </WalletProvider>
    );
    const { result } = renderHook(() => useWalletBalance(), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    // Key carries the scope — a different user (scope) can never hit it.
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger-wallet", "user-A", "balances", ""] }),
    ).toBeDefined();
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger-wallet", "user-B", "balances", ""] }),
    ).toBeUndefined();
  });

  test("useWalletHolds lists under ['ledger-wallet','','holds']", async () => {
    const qc = new QueryClient();
    server.use(
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({
          code: 200,
          message: "ok",
          data: {
            list: [
              {
                uid: "r-1",
                amount: "25",
                currency_uid: "cur-1",
                currency_code: "USD",
                created_at: "2026-07-08T02:00:00Z",
                expires_at: "2026-07-08T03:00:00Z",
              },
            ],
          },
        }),
      ),
    );
    const { result } = renderHook(() => useWalletHolds(), {
      wrapper: wrapperWith(qc),
    });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data?.[0].amount).toBe("25");
    expect(
      qc.getQueryCache().find({ queryKey: ["ledger-wallet", "", "holds"] }),
    ).toBeDefined();
  });
});
