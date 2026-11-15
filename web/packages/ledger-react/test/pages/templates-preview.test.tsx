import { fireEvent, screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test, vi } from "vitest";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { TemplatesPage as HerouiTemplatesPage } from "../../src/heroui/pages/TemplatesPage";
import { renderPage, server, getOk, BASE } from "./render-page";

function template() {
  return {
    uid: "tpl-1",
    code: "settle",
    name: "Settlement",
    journal_type_uid: "jt-1",
    is_active: true,
    lines: [
      { classification_uid: "cls-1", entry_type: "debit", holder_role: "user", amount_key: "amount", sort_order: 1 },
      { classification_uid: "cls-2", entry_type: "credit", holder_role: "system", amount_key: "amount", sort_order: 2 },
    ],
    created_at: "2026-01-01T00:00:00Z",
  };
}

function seedMetadata() {
  server.use(
    getOk("/api/v1/templates", [template()]),
    getOk("/api/v1/classifications", []),
    getOk("/api/v1/currencies", [{ uid: "cur-1", code: "USD", name: "US Dollar", is_active: true, exponent: 2 }]),
    getOk("/api/v1/journal-types", []),
  );
}

// J-7 (2026-09-02 web audit): the server's previewTemplateResponse has only
// `entries` (server/handler_metadata.go) — no total_debit/total_credit.
// PreviewResult used to claim both as required strings, so this line
// rendered "Total Debit: undefined | Total Credit: undefined" for every
// preview. Pin: the totals are the SUM of entries by entry_type, not the
// literal string "undefined".
describe.each([
  ["shadcn", TemplatesPage],
  ["heroui", HerouiTemplatesPage],
])("TemplatesPage (%s) — preview totals", (_skin, Page) => {
  test("renders totals computed from entries, never 'undefined'", async () => {
    seedMetadata();
    server.use(
      http.post(`${BASE}/api/v1/templates/settle/preview`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          data: {
            entries: [
              { account_holder: 1001, currency_uid: "cur-1", classification_uid: "cls-1", entry_type: "debit", amount: "12.50" },
              { account_holder: -1, currency_uid: "cur-1", classification_uid: "cls-2", entry_type: "credit", amount: "12.50" },
            ],
          },
        }),
      ),
    );
    renderPage(<Page />);
    await waitFor(() => expect(screen.getByText("Settlement")).toBeInTheDocument());
    // First "Preview" toggles the row expanded (becomes "Collapse"); the
    // PreviewSection that appears has its own "Preview" submit button.
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Collapse" })).toBeInTheDocument());
    fireEvent.change(screen.getByRole("textbox", { name: "Holder ID" }), {
      target: { value: "1001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));

    await waitFor(() =>
      expect(screen.getByText(/Total Debit:/)).toBeInTheDocument(),
    );
    const text = screen.getByText(/Total Debit:/).textContent ?? "";
    expect(text).not.toContain("undefined");
    expect(text).toContain("12.5000"); // formatAmount("12.50") banding
  });

  test("retains duplicate postings and their DOM rows when unrelated entries reorder", async () => {
    seedMetadata();
    const debit = Object.freeze({ account_holder: 1001, currency_uid: "cur-1", classification_uid: "cls-1", entry_type: "debit", amount: "12.50" });
    const credit = Object.freeze({ account_holder: -1001, currency_uid: "cur-1", classification_uid: "cls-2", entry_type: "credit", amount: "25.00" });
    let entries = [debit, credit, debit];
    let requests = 0;
    const consoleError = vi.spyOn(console, "error");
    server.use(http.post(`${BASE}/api/v1/templates/settle/preview`, () => {
      requests++;
      return HttpResponse.json({ code: 200, message: null, data: { entries } });
    }));
    renderPage(<Page />);
    await screen.findByText("Settlement");
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    fireEvent.change(screen.getByRole("textbox", { name: "Holder ID" }), { target: { value: "1001" } });
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    const creditText = "CREDIT holder=-1001 class=cls-2 cur=cur-1 amt=25.00";
    const debitText = "DEBIT holder=1001 class=cls-1 cur=cur-1 amt=12.50";
    const creditRow = await screen.findByText(creditText);
    const debitRows = screen.getAllByText(debitText);
    expect(debitRows).toHaveLength(2);
    expect(screen.getByText(/Total Debit:/)).toHaveTextContent("25.0000");

    entries = [credit, debit, debit];
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    await waitFor(() => {
      expect(requests).toBe(2);
      expect(creditRow.nextElementSibling).toBe(debitRows[0]);
    });
    expect(screen.getByText(creditText)).toBe(creditRow);
    expect(screen.getAllByText(debitText)).toEqual(debitRows);
    expect(consoleError.mock.calls.some((args) => args.some((arg) => typeof arg === "string" && /same key|unique.*key/.test(arg)))).toBe(false);
    consoleError.mockRestore();
  });
});
