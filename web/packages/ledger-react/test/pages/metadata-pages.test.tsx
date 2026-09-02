import { fireEvent, screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test, vi } from "vitest";
import { toast } from "sonner";
import { toast as herouiToast } from "@heroui/react";
import { CurrenciesPage } from "../../src/components/pages/CurrenciesPage";
import { ClassificationsPage } from "../../src/components/pages/ClassificationsPage";
import { ClassificationsPage as HerouiClassificationsPage } from "../../src/heroui/pages/ClassificationsPage";
import { JournalTypesPage } from "../../src/components/pages/JournalTypesPage";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { renderPage, server, getOk, BASE } from "./render-page";

// J-8 (2026-09-02 web audit): server-side field-level validation errors
// (api-contract.md §1's message.fields) used to collapse into the same
// generic toast as any other error — no page read `err.apiError.fields`.
// Pin: a create failure with `fields` renders the field-specific message
// inline (not a toast), on both skins.
describe.each([
  ["shadcn", ClassificationsPage],
  ["heroui", HerouiClassificationsPage],
])("ClassificationsPage (%s) — create field errors", (_skin, Page) => {
  test("a 'code already exists' field error renders inline, not as a toast", async () => {
    const errorSpy = vi.spyOn(toast, "error").mockImplementation(() => "" as never);
    const dangerSpy = vi.spyOn(herouiToast, "danger").mockImplementation(() => "" as never);
    server.use(
      getOk("/api/v1/classifications", []),
      http.post(`${BASE}/api/v1/classifications`, () =>
        HttpResponse.json(
          {
            code: 12003,
            message: { text: "invalid input", fields: { code: "a classification with this code already exists" } },
            data: null,
          },
          { status: 400 },
        ),
      ),
    );
    renderPage(<Page />);
    fireEvent.click(screen.getByRole("button", { name: "Create" }));
    const codeInput = await screen.findByPlaceholderText("main_wallet");
    fireEvent.change(codeInput, { target: { value: "main_wallet" } });
    fireEvent.change(screen.getByPlaceholderText("Main Wallet"), { target: { value: "Main Wallet" } });
    const submitButtons = await screen.findAllByRole("button", { name: "Create" });
    fireEvent.click(submitButtons[submitButtons.length - 1]);

    await waitFor(() =>
      expect(
        screen.getByText("a classification with this code already exists"),
      ).toBeInTheDocument(),
    );
    expect(errorSpy).not.toHaveBeenCalled();
    expect(dangerSpy).not.toHaveBeenCalled();
  });
});

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
      // TemplatesPage resolves classification uids to human codes via
      // useUidCodeLookups, so both metadata lists must be stubbed too.
      getOk("/api/v1/classifications", [
        { uid: "cls-1", code: "main_wallet", name: "Main Wallet", normal_side: "debit", is_system: false, is_active: true },
        { uid: "cls-2", code: "custodial", name: "Custodial", normal_side: "credit", is_system: true, is_active: true },
      ]),
      getOk("/api/v1/currencies", [
        { uid: "cur-1", code: "USDT", name: "Tether USD", is_active: true },
      ]),
      getOk("/api/v1/templates", [
        {
          id: 1,
          code: "deposit_confirm",
          name: "Confirm Deposit",
          journal_type_uid: 1,
          is_active: true,
          lines: [
            { id: 1, classification_uid: "cls-1", entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
            { id: 2, classification_uid: "cls-2", entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
          ],
        },
      ]),
    );
    renderPage(<TemplatesPage />);
    expect(screen.getByRole("heading", { name: "Templates" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("Confirm Deposit")).toBeInTheDocument());
    expect(screen.getByText("deposit_confirm")).toBeInTheDocument();
    // Line rows show the human classification code, not the raw uid.
    await waitFor(() =>
      expect(screen.getByText(/main_wallet \/ user/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/cls-1/)).not.toBeInTheDocument();
  });
});
