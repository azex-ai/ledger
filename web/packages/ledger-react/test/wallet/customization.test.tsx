import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import {
  WalletBalanceCard,
  WalletBalances,
  WalletPanel,
  WalletProvider,
  type WalletBalance,
  type WalletHold,
  type WalletTransaction,
} from "../../src/wallet";
import { server } from "../setup";

const BASE = "http://wallet.test/api/v1";
const balance: WalletBalance = {
  currency_uid: "usdc",
  currency_code: "USDC",
  available: "1000.011234567890123456",
  pending: "0.01",
  locked: "0.07",
  total: "1000.091234567890123456",
};
const transaction: WalletTransaction = {
  uid: "purchase",
  kind: "transfer",
  kind_label: "Transfer",
  direction: "out",
  amount: "1.000001",
  currency_uid: "usdc",
  currency_code: "USDC",
  occurred_at: "2026-09-06T00:00:00Z",
  reversal_of_uid: "",
  memo: "Credits purchase",
};

function ok<T>(data: T) {
  return HttpResponse.json({ code: 200, message: null, data });
}

function seed(balances: WalletBalance[] = []) {
  server.use(
    http.get(`${BASE}/holder/balances`, () => ok({ list: balances })),
    http.get(`${BASE}/holder/holds`, () => ok({ list: [] })),
    http.get(`${BASE}/holder/transactions`, () => ok({ list: [], next_cursor: "" })),
  );
}

function wrap(children: ReactNode, fetch?: typeof globalThis.fetch) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return (
    <WalletProvider config={{ baseUrl: BASE, queryClient, fetch }}>
      {children}
    </WalletProvider>
  );
}

describe("shadcn wallet composition", () => {
  test.each([
    ["panel", WalletPanel],
    ["balances", WalletBalances],
    ["single balance", WalletBalanceCard],
  ])("keeps a working first top-up action in an empty %s", async (_name, Component) => {
    seed();
    const topUp = vi.fn();
    render(wrap(<Component actions={<button onClick={topUp}>Top up</button>} />));

    await screen.findByText("No balance yet");
    fireEvent.click(screen.getByRole("button", { name: "Top up" }));
    expect(topUp).toHaveBeenCalledOnce();
  });

  test("passes null to an action renderer before the first balance exists", async () => {
    seed();
    const actions = vi.fn((current: WalletBalance | null) => (
      <button>{current ? `Top up ${current.currency_code}` : "Make your first deposit"}</button>
    ));
    render(wrap(<WalletPanel actions={actions} />));

    await screen.findByRole("button", { name: "Make your first deposit" });
    expect(actions).toHaveBeenCalledWith(null);
  });

  test("binds each action to its own currency and full balance", async () => {
    const credits: WalletBalance = {
      currency_uid: "credits",
      currency_code: "CREDITS",
      available: "999.5",
      pending: "0",
      locked: "0.5",
      total: "1000",
    };
    seed([balance, credits]);
    const choose = vi.fn();
    render(wrap(
      <WalletPanel actions={(current) => current && (
        <button onClick={() => choose(current)}>{`Use ${current.currency_code}`}</button>
      )} />,
    ));

    fireEvent.click(await screen.findByRole("button", { name: "Use USDC" }));
    fireEvent.click(screen.getByRole("button", { name: "Use CREDITS" }));
    expect(choose.mock.calls).toEqual([[balance], [credits]]);
  });

  test("replaces the balance region without mounting its queries and keeps default activity", async () => {
    const fetch = vi.fn(globalThis.fetch);
    server.use(http.get(`${BASE}/holder/transactions`, () => ok({ list: [transaction], next_cursor: "" })));
    render(wrap(<WalletPanel slots={{ balances: <section>My credits summary</section> }} />, fetch));

    expect(screen.getByText("My credits summary")).toBeInTheDocument();
    await screen.findByText("Credits purchase", { exact: false });
    expect(screen.getByRole("region", { name: "Transaction history" })).toBeInTheDocument();
    expect(screen.queryByText("USDC balance")).not.toBeInTheDocument();
    expect(fetch).toHaveBeenCalledOnce();
    expect(fetch.mock.calls[0][0]).toBe(`${BASE}/holder/transactions?limit=20`);
  });

  test("null slots hide complete regions and issue no default requests", () => {
    const fetch = vi.fn<typeof globalThis.fetch>();
    render(wrap(<WalletPanel slots={{ balances: null, transactions: null }} />, fetch));

    expect(screen.queryByText("Activity")).not.toBeInTheDocument();
    expect(screen.queryByText("No balance yet")).not.toBeInTheDocument();
    expect(fetch).not.toHaveBeenCalled();
  });

  test("forwards custom transaction rendering and page size through cursor pagination", async () => {
    const requests: { limit: string | null; cursor: string | null }[] = [];
    server.use(http.get(`${BASE}/holder/transactions`, ({ request }) => {
      const params = new URL(request.url).searchParams;
      const cursor = params.get("cursor");
      requests.push({ limit: params.get("limit"), cursor });
      return ok({
        list: [{ ...transaction, uid: cursor ? "usage" : "purchase" }],
        next_cursor: cursor ? "" : "next-page",
      });
    }));
    render(wrap(
      <WalletPanel
        slots={{ balances: null }}
        limit={1}
        renderItem={(tx) => <span>{`Custom ${tx.uid}: ${tx.amount} ${tx.currency_code}`}</span>}
      />,
    ));

    await screen.findByText("Custom purchase: 1.000001 USDC");
    fireEvent.click(screen.getByRole("button", { name: "Load More" }));
    await screen.findByText("Custom usage: 1.000001 USDC");
    expect(screen.getByText("Custom purchase: 1.000001 USDC")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Load More" })).not.toBeInTheDocument();
    expect(requests).toEqual([
      { limit: "1", cursor: null },
      { limit: "1", cursor: "next-page" },
    ]);
  });
});

describe("shadcn wallet exact amounts", () => {
  test("preserves display bands and exposes the full total, available, pending and on-hold values", async () => {
    seed([balance]);
    render(wrap(<WalletBalances />));

    const total = await screen.findByRole("button", {
      name: `Exact amount: ${balance.total} USDC`,
    });
    expect(total).toHaveTextContent("1,000.0");
    expect(screen.getByRole("button", {
      name: `Exact amount: ${balance.available} USDC`,
    })).toHaveTextContent("1,000.0");
    expect(screen.getByRole("button", { name: "Exact amount: 0.01 USDC" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Exact amount: 0.07 USDC" })).toBeInTheDocument();

    fireEvent.click(total);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(`${balance.total} USDC`);
  });

  test("keyboard focus exposes an outgoing transaction's exact signed amount and Escape closes it", async () => {
    server.use(http.get(`${BASE}/holder/transactions`, () => ok({ list: [transaction], next_cursor: "" })));
    render(wrap(<WalletPanel slots={{ balances: null }} />));

    const amount = await screen.findByRole("button", { name: "Exact amount: -1.000001 USDC" });
    expect(amount).toHaveTextContent("-1.0000");
    await act(async () => amount.focus());
    expect(await screen.findByRole("tooltip")).toHaveTextContent("-1.000001 USDC");
    fireEvent.keyDown(amount, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("tooltip")).not.toBeInTheDocument());
  });

  test("exposes exact fractional credits in expanded hold details", async () => {
    const held = "0.000000000000000001";
    const hold: WalletHold = {
      uid: "budget",
      amount: held,
      currency_uid: "credits",
      currency_code: "CREDITS",
      created_at: "2026-09-06T00:00:00Z",
      expires_at: "2026-09-07T00:00:00Z",
    };
    seed([{
      currency_uid: "credits", currency_code: "CREDITS",
      available: "1000", pending: "0", locked: held, total: "1000.000000000000000001",
    }]);
    server.use(http.get(`${BASE}/holder/holds`, () => ok({ list: [hold] })));
    render(wrap(<WalletBalances />));

    fireEvent.click(await screen.findByRole("button", { name: "On hold" }));
    const exactAmounts = await screen.findAllByRole("button", { name: `Exact amount: ${held} CREDITS` });
    expect(exactAmounts).toHaveLength(2);
    fireEvent.click(exactAmounts[1]);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(`${held} CREDITS`);
  });
});
