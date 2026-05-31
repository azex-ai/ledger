import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { CurrenciesPage } from "../../src/components/pages/CurrenciesPage";
import { ClassificationsPage } from "../../src/components/pages/ClassificationsPage";
import { JournalTypesPage } from "../../src/components/pages/JournalTypesPage";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { renderPage, server, getOk } from "./render-page";

describe("CurrenciesPage", () => {
  test("renders heading and currency rows with a Deactivate action", async () => {
    server.use(
      getOk("/api/v1/currencies", [
        { id: 1, code: "USDT", name: "Tether USD", is_active: true },
      ]),
    );
    renderPage(<CurrenciesPage />);
    expect(screen.getByRole("heading", { name: "Currencies" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("USDT")).toBeInTheDocument());
    expect(screen.getByText("Tether USD")).toBeInTheDocument();
    // Deactivate action present for active currencies.
    expect(screen.getByRole("button", { name: "Deactivate" })).toBeInTheDocument();
  });
});

describe("ClassificationsPage", () => {
  test("renders heading and classification rows", async () => {
    server.use(
      getOk("/api/v1/classifications", [
        { id: 1, code: "main_wallet", name: "Main Wallet", normal_side: "debit", is_system: false, is_active: true },
      ]),
    );
    renderPage(<ClassificationsPage />);
    expect(screen.getByRole("heading", { name: "Classifications" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("main_wallet")).toBeInTheDocument());
    expect(screen.getByText("Main Wallet")).toBeInTheDocument();
  });
});

describe("JournalTypesPage", () => {
  test("renders heading and journal type rows", async () => {
    server.use(
      getOk("/api/v1/journal-types", [
        { id: 1, code: "deposit", name: "Deposit Confirmation", is_active: true, created_at: "2026-01-01T00:00:00Z" },
      ]),
    );
    renderPage(<JournalTypesPage />);
    expect(screen.getByRole("heading", { name: "Journal Types" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("deposit")).toBeInTheDocument());
    expect(screen.getByText("Deposit Confirmation")).toBeInTheDocument();
  });
});

describe("TemplatesPage", () => {
  test("renders heading and template cards", async () => {
    server.use(
      getOk("/api/v1/templates", [
        {
          id: 1,
          code: "deposit_confirm",
          name: "Confirm Deposit",
          journal_type_id: 1,
          is_active: true,
          lines: [
            { id: 1, classification_id: 1, entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
            { id: 2, classification_id: 2, entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
          ],
        },
      ]),
    );
    renderPage(<TemplatesPage />);
    expect(screen.getByRole("heading", { name: "Templates" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Confirm Deposit")).toBeInTheDocument());
    expect(screen.getByText("deposit_confirm")).toBeInTheDocument();
  });
});
