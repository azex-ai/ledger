import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, expect, test } from "vitest";
import { http } from "msw";
import { ExactAmount } from "../../src/components/exact-amount";
import { journalTotalsByCurrency } from "../../src/lib/journal-totals";
import { JournalDetailPage } from "../../src/components/pages/JournalDetailPage";
import { BalancesPage } from "../../src/components/pages/BalancesPage";
import { CurrenciesPage } from "../../src/components/pages/CurrenciesPage";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { ClassificationsPage } from "../../src/components/pages/ClassificationsPage";
import { BalancesPage as HeroBalances } from "../../src/heroui/pages/BalancesPage";
import { JournalDetailPage as HeroJournal } from "../../src/heroui/pages/JournalDetailPage";
import { BASE, getOk, ok, renderPage, server } from "./render-page";
import type { Entry } from "../../src/client/types";

const currencies = [{ uid: "usd", code: "USD", name: "US Dollar", exponent: 2, is_active: true }, { uid: "btc", code: "BTC", name: "Bitcoin", exponent: 8, is_active: true }];
beforeEach(() => server.use(getOk("/api/v1/currencies", currencies), getOk("/api/v1/classifications", []), getOk("/api/v1/journal-types", [])));
afterEach(cleanup);
function entry(currency_uid: string, amount: string, entry_type: "debit" | "credit", classification_uid = "wallet"): Entry {
  return { journal_uid: "j1", account_holder: entry_type === "debit" ? 1001 : -1, currency_uid, classification_uid, amount, entry_type, created_at: "2026-10-10T00:00:00Z", effective_at: "2026-10-10T00:00:00Z" };
}
const journal = { uid: "j1", journal_type_uid: "jt1", idempotency_key: "event1", total_debit: "1.23000002", total_credit: "1.23000002", metadata: {}, actor_id: 0, source: "test", created_at: "2026-10-10T00:00:00Z", effective_at: "2026-10-10T00:00:00Z" };

test("exact amount is focusable and touch/click exposes the original wire string", async () => {
  const raw = "999999999999.123456789012345678";
  render(<ExactAmount value={raw} currencyCode="ATOM" />);
  const trigger = screen.getByRole("button", { name: `Exact amount: ${raw} ATOM` });
  expect(trigger.textContent).toBe("999,999,999,999.1");
  trigger.focus();
  expect(document.activeElement).toBe(trigger);
  fireEvent.click(trigger);
  expect(await screen.findByRole("tooltip")).toHaveTextContent(`${raw} ATOM`);
});

test("journal totals preserve 18 decimal places and never add unlike currencies", () => {
  expect(journalTotalsByCurrency([
    entry("atom", "999999999999.123456789012345678", "debit", "one"),
    entry("atom", "0.000000000000000001", "debit", "two"),
    entry("btc", "0.00000002", "credit"),
    entry("usd", "1.23", "debit"),
  ])).toEqual([
    { currencyUid: "atom", debit: "999999999999.123456789012345679", credit: "0" },
    { currencyUid: "btc", debit: "0", credit: "0.00000002" },
    { currencyUid: "usd", debit: "1.23", credit: "0" },
  ]);
});

test.each([["shadcn", JournalDetailPage], ["heroui", HeroJournal]] as const)("%s detail consumes the real flat response", async (_skin, Page) => {
  server.use(getOk("/api/v1/journals/j1", { ...journal, entries: [entry("usd", "1.23", "debit"), entry("usd", "1.23", "credit"), entry("btc", "0.00000002", "debit"), entry("btc", "0.00000002", "credit")] }));
  renderPage(<Page id="j1" />);
  expect(await screen.findByRole("heading", { name: "Journal #j1" })).toBeInTheDocument();
  if (_skin === "shadcn") {
    const totals = screen.getByRole("table", { name: "Journal totals by currency" });
    expect(within(totals).getAllByRole("button", { name: "Exact amount: 1.23 USD" })).toHaveLength(2);
    expect(within(totals).getAllByRole("button", { name: "Exact amount: 0.00000002 BTC" })).toHaveLength(2);
    expect(screen.queryByText("Fund Flow")).not.toBeInTheDocument();
    expect(within(screen.getByRole("table", { name: "Journal entries" })).queryByRole("columnheader", { name: "ID" })).not.toBeInTheDocument();
    expect(screen.queryByText("1.23000002")).not.toBeInTheDocument();
  }
});

test.each([["shadcn", BalancesPage], ["heroui", HeroBalances]] as const)("%s booked balances and snapshots use the selected currency without a cross-currency request", async (skin, Page) => {
  const requested: string[] = [];
  server.use(http.get(`${BASE}/api/v1/balances/1001`, ({ request }) => {
    const currency = new URL(request.url).searchParams.get("currency_uid");
    expect(currency).toMatch(/^(usd|btc)$/);
    requested.push(currency!);
    return ok({ list: [{ account_holder: 1001, currency_uid: currency, classification_uid: "wallet", balance: currency === "usd" ? "1234.56" : "0.00000001" }] });
  }), getOk("/api/v1/snapshots", [], { requiredQuery: ["holder", "currency_uid", "start", "end"] }));
  renderPage(<Page />);
  fireEvent.change(screen.getByLabelText("Account Holder ID"), { target: { value: "1001" } });
  fireEvent.click(screen.getByRole("button", { name: "Search" }));
  if (skin === "shadcn") expect(await screen.findByRole("button", { name: "Exact amount: 1234.56 USD" })).toBeInTheDocument();
  else expect(await screen.findByText("1,234.5")).toBeInTheDocument();
  expect(screen.getByText(/reservation holds are not deducted/)).toBeInTheDocument();
  fireEvent.click(screen.getByLabelText("Currency"));
  const btcOption = await screen.findByRole("option", { name: "BTC" });
  fireEvent.pointerDown(btcOption, { pointerType: "mouse" });
  fireEvent.click(btcOption);
  if (skin === "shadcn") expect(await screen.findByRole("button", { name: "Exact amount: 0.00000001 BTC" })).toBeInTheDocument();
  else expect(await screen.findByText("0.0₇1000")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Exact amount: 1234.56 USD" })).not.toBeInTheDocument();
  expect(screen.getByText(/Booked balance trend.*BTC/)).toBeInTheDocument();
  expect(requested).toEqual(["usd", "btc"]);
});

test("currency create rejects fractional/out-of-range exponents and accepts zero", async () => {
  const posted: unknown[] = [];
  server.use(http.post(`${BASE}/api/v1/currencies`, async ({ request }) => { posted.push(await request.json()); return ok({ ...currencies[0], uid: "points", code: "POINTS", exponent: 0 }); }));
  renderPage(<CurrenciesPage />);
  expect(await screen.findByRole("columnheader", { name: "Decimal places" })).toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "Create" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Code"), { target: { value: "POINTS" } });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Points" } });
  for (const value of ["2.5", "19", "-1"]) {
    fireEvent.change(within(dialog).getByLabelText("Decimal places (0-18)"), { target: { value } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
    expect(within(dialog).getByText("Enter a whole number from 0 to 18.")).toBeInTheDocument();
    expect(posted).toHaveLength(0);
  }
  fireEvent.change(within(dialog).getByLabelText("Decimal places (0-18)"), { target: { value: "0" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
  await waitFor(() => expect(posted).toEqual([{ code: "POINTS", name: "Points", exponent: 0 }]));
});

test("system classification is an explicit configuration choice", async () => {
  const posted: Record<string, unknown>[] = [];
  server.use(http.post(`${BASE}/api/v1/classifications`, async ({ request }) => { posted.push(await request.json() as Record<string, unknown>); return ok({ uid: "system" }); }));
  renderPage(<ClassificationsPage />);
  fireEvent.click(screen.getByRole("button", { name: "Create" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Code"), { target: { value: "custodial" } });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Custodial" } });
  fireEvent.click(within(dialog).getByLabelText("Account owner"));
  const systemOption = await screen.findByRole("option", { name: "System" });
  fireEvent.pointerDown(systemOption, { pointerType: "mouse" });
  fireEvent.click(systemOption);
  fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
  await waitFor(() => expect(posted[0]?.is_system).toBe(true));
});


test("template creation exposes usable named controls for both debit and credit lines", async () => {
  let submitted: unknown;
  server.use(
    getOk("/api/v1/templates", []),
    getOk("/api/v1/journal-types", [{ uid: "jt", code: "deposit", name: "Deposit", is_active: true }]),
    getOk("/api/v1/classifications", [{ uid: "wallet", code: "wallet", name: "Wallet", normal_side: "debit", balance_role: "available", is_system: false, is_active: true }, { uid: "custodial", code: "custodial", name: "Custodial", normal_side: "credit", balance_role: "available", is_system: true, is_active: true }]),
    http.post(`${BASE}/api/v1/templates`, async ({ request }) => { submitted = await request.json(); return ok({ uid: "created" }); }),
  );
  renderPage(<TemplatesPage />);
  fireEvent.click(screen.getByRole("button", { name: "Create Template" }));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Code"), { target: { value: "deposit" } });
  fireEvent.change(within(dialog).getByLabelText("Name"), { target: { value: "Deposit" } });
  async function select(label: string, name: string) {
    fireEvent.click(within(dialog).getByLabelText(label));
    const option = await screen.findByRole("option", { name });
    fireEvent.pointerDown(option, { pointerType: "mouse" });
    fireEvent.click(option);
  }
  await select("Journal Type", "Deposit");
  await select("Debit classification 1", "wallet");
  await select("Credit classification 2", "custodial");
  expect(within(dialog).getByLabelText("Debit holder 1")).toHaveTextContent("User");
  expect(within(dialog).getByLabelText("Credit holder 2")).toHaveTextContent("System");
  fireEvent.change(within(dialog).getByLabelText("Debit amount key 1"), { target: { value: "principal" } });
  fireEvent.change(within(dialog).getByLabelText("Credit amount key 2"), { target: { value: "principal" } });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create" }));
  await waitFor(() => expect(submitted).toEqual({ code: "deposit", name: "Deposit", journal_type_uid: "jt", lines: [
    { classification_uid: "wallet", entry_type: "debit", holder_role: "user", amount_key: "principal", sort_order: 1 },
    { classification_uid: "custodial", entry_type: "credit", holder_role: "system", amount_key: "principal", sort_order: 2 },
  ] }));
});
