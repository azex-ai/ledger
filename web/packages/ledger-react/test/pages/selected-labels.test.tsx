import { fireEvent, screen, waitFor } from "@testing-library/react";
import { beforeEach, expect, test } from "vitest";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { JournalsPage } from "../../src/components/pages/JournalsPage";
import { getOk, renderPage, server } from "./render-page";

const journalTypeUID = "019a0000-0000-7000-8000-000000000001";
const currencyUID = "019a0000-0000-7000-8000-000000000002";
const templateCode = "deposit_confirm";
beforeEach(() => server.use(
  getOk("/api/v1/journals", { list: [], next_cursor: "" }),
  getOk("/api/v1/journal-types", [{ uid: journalTypeUID, code: "deposit", name: "Customer deposit", is_active: true }]),
  getOk("/api/v1/currencies", [{ uid: currencyUID, code: "USD", name: "US Dollar", exponent: 2, is_active: true }]),
  getOk("/api/v1/classifications", []),
  getOk("/api/v1/templates", [{ uid: "template-1", code: templateCode, name: "Confirm deposit", journal_type_uid: journalTypeUID, lines: [], is_active: true }]),
));

async function selectAndClose(label: string, display: string, raw: string) {
  const trigger = screen.getByRole("combobox", { name: label });
  fireEvent.click(trigger);
  const option = await screen.findByRole("option", { name: display });
  fireEvent.pointerDown(option, { pointerType: "mouse" });
  fireEvent.click(option);
  await waitFor(() => expect(screen.queryByRole("listbox")).not.toBeInTheDocument());
  expect(trigger).toHaveTextContent(display);
  expect(trigger).not.toHaveTextContent(raw);
}

test("template creation retains the selected journal type name after closing options", async () => {
  renderPage(<TemplatesPage />);
  fireEvent.click(screen.getByRole("button", { name: "Create Template" }));
  await selectAndClose("Journal Type", "Customer deposit", journalTypeUID);
});

test("manual journal retains the selected journal type name after closing options", async () => {
  renderPage(<JournalsPage />);
  fireEvent.click(screen.getByRole("button", { name: "Post Journal" }));
  await selectAndClose("Journal Type", "Customer deposit", journalTypeUID);
});

test("template journal retains template name and currency code instead of identifiers", async () => {
  renderPage(<JournalsPage />);
  fireEvent.click(screen.getByRole("button", { name: "Template Journal" }));
  await selectAndClose("Template", "Confirm deposit", templateCode);
  await selectAndClose("Currency", "USD", currencyUID);
  expect(screen.getByRole("combobox", { name: "Template" })).toHaveTextContent("Confirm deposit");
});

test("template preview retains the selected currency code after closing options", async () => {
  renderPage(<TemplatesPage />);
  fireEvent.click(await screen.findByRole("button", { name: "Preview" }));
  await selectAndClose("Currency", "USD", currencyUID);
});
