import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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
        message: null,
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
        message: null,
        data: {
          list: [
            {
              uid: "j-1",
              kind: "deposit",
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
              kind: "deposit",
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
        message: null,
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
  ["shadcn", () => <WalletPanel kindLabels={{ deposit: "Top up" }} />],
  ["heroui", () => <HerouiWalletPanel kindLabels={{ deposit: "Top up" }} />],
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
          { code: 19999, message: { text: "Internal server error" }, data: null },
          { status: 500 },
        ),
      ),
      http.get(`${BASE}/holder/transactions`, () =>
        HttpResponse.json(
          { code: 19999, message: { text: "Internal server error" }, data: null },
          { status: 500 },
        ),
      ),
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({ code: 200, message: null, data: { list: [] } }),
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

  // J-3 (2026-09-02 web audit): the holds query's isLoading/isError were
  // never read, so a failed /holder/holds request rendered the same
  // "Nothing is on hold right now." as a genuinely empty result —
  // contradicting the non-zero `locked` figure shown one row up on the same
  // card (a real balance, real holds request, real failure).
  test("shows an error state (not a false empty state) when holds fails while locked > 0", async () => {
    server.use(
      http.get(`${BASE}/holder/balances`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
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
        HttpResponse.json({ code: 200, message: null, data: { list: [], next_cursor: "" } }),
      ),
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({ code: 19999, message: { text: "boom" }, data: null }, { status: 500 }),
      ),
    );
    render(wrap(<Panel />));
    await waitFor(() =>
      expect(screen.getByText("CREDITS balance")).toBeInTheDocument(),
    );
    fireEvent.click(screen.getByRole("button", { name: /On hold/i }));
    await waitFor(() =>
      expect(screen.getByText(/Couldn.t load hold details/i)).toBeInTheDocument(),
    );
    expect(
      screen.queryByText("Nothing is on hold right now."),
    ).not.toBeInTheDocument();
  });

  // J-21 (2026-09-02 web audit): the row used to hardcode "+"/"-" at the call
  // site from `direction` instead of going through `formatSignedAmount` (the
  // package's single sign/color authority everywhere else, e.g.
  // ReconciliationPage's drift cell). This wasn't just a structural
  // inconsistency: a zero-amount "out" transaction hardcoded to "-0.00" is a
  // genuine display bug against display.ts's own pinned convention ("negative
  // zero displays as the unsigned zero form", test/lib/display.test.ts) — the
  // row had two different zero conventions depending on `direction`.
  test("a zero-amount direction=out row renders the unsigned zero form, not a hardcoded minus", async () => {
    server.use(
      http.get(`${BASE}/holder/balances`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
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
          message: null,
          data: {
            list: [
              {
                uid: "j-9",
                kind: "withdrawal",
                kind_label: "Withdrawal",
                direction: "out",
                amount: "0", // absolute value per contract — direction carries the sign
                currency_uid: "cur-1",
                currency_code: "CREDITS",
                occurred_at: "2026-07-08T02:00:00Z",
                reversal_of_uid: "",
                memo: "",
              },
            ],
            next_cursor: "",
          },
        }),
      ),
      http.get(`${BASE}/holder/holds`, () =>
        HttpResponse.json({ code: 200, message: null, data: { list: [] } }),
      ),
    );
    const { container } = render(wrap(<Panel />));
    await waitFor(() =>
      expect(screen.getByText("CREDITS balance")).toBeInTheDocument(),
    );
    expect(container.textContent).toContain("+0.00");
    expect(container.textContent).not.toContain("-0.00");
  });
});
