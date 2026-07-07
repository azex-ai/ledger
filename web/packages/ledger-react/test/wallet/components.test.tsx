import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { WalletProvider } from "../../src/wallet/provider";
import { WalletPanel } from "../../src/wallet/components/wallet-panel";
import { WalletPanel as HerouiWalletPanel } from "../../src/wallet/heroui/wallet-panel";
import { server } from "../setup";

const BASE = "http://wallet.test/api/v1";

// Realistic payloads INCLUDING a reversal row — the surface must translate,
// not leak.
function seedWalletAPI() {
  server.use(
    http.get(`${BASE}/holder/balances`, () =>
      HttpResponse.json({
        code: 200,
        message: "ok",
        data: {
          list: [
            {
              currency_uid: "cur-1",
              currency_code: "CREDITS",
              available: "75.5",
              pending: "40",
              locked: "25",
              total: "140.5",
            },
          ],
        },
      }),
    ),
    http.get(`${BASE}/holder/transactions`, () =>
      HttpResponse.json({
        code: 200,
        message: "ok",
        data: {
          list: [
            {
              uid: "j-1",
              kind: "deposit_confirm",
              kind_label: "Deposit",
              direction: "in",
              amount: "100",
              currency_uid: "cur-1",
              currency_code: "CREDITS",
              occurred_at: "2026-07-08T02:00:00Z",
              reversal_of_uid: "",
              memo: "monthly top up",
            },
            {
              uid: "j-2",
              kind: "deposit_confirm",
              kind_label: "Deposit",
              direction: "out",
              amount: "100",
              currency_uid: "cur-1",
              currency_code: "CREDITS",
              occurred_at: "2026-07-08T03:00:00Z",
              reversal_of_uid: "j-1",
              memo: "",
            },
          ],
          next_cursor: "",
        },
      }),
    ),
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
              currency_code: "CREDITS",
              created_at: "2026-07-08T02:00:00Z",
              expires_at: "2026-07-08T03:00:00Z",
            },
          ],
        },
      }),
    ),
  );
}

function wrap(children: ReactNode) {
  // retry: false — error-path tests must fail fast, not sit in react-query's
  // default retry backoff past the waitFor timeout.
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <WalletProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </WalletProvider>
  );
}

// user-facing-surfaces.md guard: double-entry vocabulary and internal
// mechanics must never reach the rendered wallet surface.
const INTERNAL_VOCABULARY = [
  /debit/i,
  /credit(?!s)/i, // "CREDITS" the currency code is user-facing; "credit" the entry side is not
  /journal/i,
  /entry/i,
  /classification/i,
  /reservation/i,
  /idempotency/i,
  /holder/i,
];

describe.each([
  ["shadcn", () => <WalletPanel kindLabels={{ deposit_confirm: "Top up" }} />],
  ["heroui", () => <HerouiWalletPanel kindLabels={{ deposit_confirm: "Top up" }} />],
])("WalletPanel (%s)", (_skin, Panel) => {
  test("renders user language and leaks no internal vocabulary", async () => {
    seedWalletAPI();
    const { container } = render(wrap(<Panel />));

    // Balance card: total + breakdown rows in user words.
    await waitFor(() =>
      expect(screen.getByText("CREDITS balance")).toBeInTheDocument(),
    );
    expect(container.textContent).toContain("140.5");
    expect(screen.getByText("Available")).toBeInTheDocument();
    expect(screen.getByText("Pending")).toBeInTheDocument();

    // Transactions: overridden label via stable kind code, refund marker,
    // signed amounts.
    await waitFor(() =>
      expect(screen.getAllByText("Top up").length).toBeGreaterThan(0),
    );
    expect(screen.getByText("Refund")).toBeInTheDocument();
    expect(screen.getByText("monthly top up", { exact: false })).toBeInTheDocument();

    const text = container.textContent ?? "";
    for (const word of INTERNAL_VOCABULARY) {
      expect(text).not.toMatch(word);
    }
  });

  test("shows a sanitized error state on API failure", async () => {
    server.use(
      http.get(`${BASE}/holder/balances`, () =>
        HttpResponse.json(
          { code: 19999, message: "pq: deadlock detected on journal_entries", data: null },
          { status: 500 },
        ),
      ),
      http.get(`${BASE}/holder/transactions`, () =>
        HttpResponse.json(
          { code: 19999, message: "pq: deadlock detected on journal_entries", data: null },
          { status: 500 },
        ),
      ),
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({ code: 200, message: "ok", data: { list: [] } }),
      ),
    );
    const { container } = render(wrap(<Panel />));
    await waitFor(() =>
      expect(
        screen.getByText("Couldn't load your balance", { exact: false }),
      ).toBeInTheDocument(),
    );
    // The raw upstream error never reaches the DOM.
    expect(container.textContent).not.toContain("deadlock");
    expect(container.textContent).not.toContain("journal_entries");
  });
});
