import { fireEvent, screen, waitFor } from "@testing-library/react";
import { http } from "msw";
import { afterEach, describe, expect, test, vi } from "vitest";
import { toast as sonner } from "sonner";
import { toast as heroToast } from "@heroui/react";
import { BalancesPage } from "../../src/components/pages/BalancesPage";
import { BalancesPage as HeroBalances } from "../../src/heroui/pages/BalancesPage";
import { SnapshotsPage } from "../../src/components/pages/SnapshotsPage";
import { SnapshotsPage as HeroSnapshots } from "../../src/heroui/pages/SnapshotsPage";
import { ReconciliationPage } from "../../src/components/pages/ReconciliationPage";
import { ReconciliationPage as HeroReconciliation } from "../../src/heroui/pages/ReconciliationPage";
import { TemplatesPage } from "../../src/components/pages/TemplatesPage";
import { TemplatesPage as HeroTemplates } from "../../src/heroui/pages/TemplatesPage";
import { JournalsPage } from "../../src/components/pages/JournalsPage";
import { JournalsPage as HeroJournals } from "../../src/heroui/pages/JournalsPage";
import { BASE, getOk, ok, renderPage, server } from "./render-page";

const invalid = ["12abc", "1.5", "1e3", "0x10", "+12", "9007199254740992", "9007199254740993", "-9007199254740992", "-9007199254740993"];
const max = "9007199254740991";
const min = "-9007199254740991";
const template = {
  uid: "tpl-1", code: "settle", name: "Settlement", journal_type_uid: "jt-1", is_active: true,
  lines: [], created_at: "2026-01-01T00:00:00Z",
};

function metadata() {
  server.use(
    getOk("/api/v1/templates", [template]),
    getOk("/api/v1/classifications", []),
    getOk("/api/v1/currencies", [{ uid: "cur-1", code: "USD", name: "US Dollar", exponent: 2, is_active: true }]),
    getOk("/api/v1/journal-types", []),
    getOk("/api/v1/journals", []),
  );
}

function change(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

async function select(label: string, option: string) {
  fireEvent.click(screen.getByLabelText(label));
  fireEvent.click(await screen.findByRole("option", { name: option }));
}

afterEach(() => vi.restoreAllMocks());

describe.each([
  { skin: "shadcn", Balances: BalancesPage, Snapshots: SnapshotsPage, Reconciliation: ReconciliationPage, Templates: TemplatesPage, Journals: JournalsPage },
  { skin: "heroui", Balances: HeroBalances, Snapshots: HeroSnapshots, Reconciliation: HeroReconciliation, Templates: HeroTemplates, Journals: HeroJournals },
])("$skin real holder inputs", ({ skin, Balances, Snapshots, Reconciliation, Templates, Journals }) => {
  function errorSpy() {
    return skin === "shadcn" ? vi.spyOn(sonner, "error") : vi.spyOn(heroToast, "danger");
  }

  test("balances reject bad text, preserve blank/zero sentinel and query exact signed safe boundaries", async () => {
    const errors = errorSpy();
    const holders: string[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    server.use(http.get(`${BASE}/api/v1/balances/:holder`, ({ params }) => {
      holders.push(String(params.holder));
      return ok({ list: [] });
    }));
    renderPage(<Balances />);
    for (const input of invalid) {
      errors.mockClear();
      change("Account Holder ID", input);
      fireEvent.click(screen.getByRole("button", { name: "Search" }));
      expect(errors).toHaveBeenCalledTimes(1);
      expect(errors).toHaveBeenLastCalledWith(expect.stringMatching(/Holder ID/));
    }
    // Keyboard submits use exactly the same validation.
    change("Account Holder ID", "12abc");
    fireEvent.keyDown(screen.getByLabelText("Account Holder ID"), { key: "Enter" });
    expect(errors).toHaveBeenCalledTimes(2);
    for (const input of ["", " \t ", "0"]) {
      change("Account Holder ID", input);
      fireEvent.click(screen.getByRole("button", { name: "Search" }));
    }
    expect(fetchSpy).not.toHaveBeenCalled();
    for (const input of [min, max, " -0012 "]) {
      change("Account Holder ID", input);
      fireEvent.click(screen.getByRole("button", { name: "Search" }));
      await waitFor(() => expect(holders).toContain(String(Number(input))));
    }
    expect(holders).toEqual([min, max, "-12"]);
  });

  test("snapshots reject invalid/zero holders and query a negative system account", async () => {
    metadata();
    const errors = vi.spyOn(sonner, "error");
    const holders: string[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    server.use(http.get(`${BASE}/api/v1/snapshots`, ({ request }) => {
      holders.push(new URL(request.url).searchParams.get("holder")!);
      return ok({ list: [] });
    }));
    renderPage(<Snapshots />);
    change("Currency", "cur-1");
    change("Start Date", "2026-10-01");
    change("End Date", "2026-10-10");
    for (const input of [...invalid, "0", " "]) {
      errors.mockClear();
      change("Holder", input);
      fireEvent.click(screen.getByRole("button", { name: "Search" }));
      expect(errors).toHaveBeenCalledTimes(1);
      expect(errors).toHaveBeenLastCalledWith(expect.stringMatching(/Holder/));
    }
    expect(fetchSpy.mock.calls.some(([url]) => String(url).includes("/snapshots"))).toBe(false);
    change("Holder", min);
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(holders).toEqual([min]));
  });

  test("account reconciliation rejects invalid/zero holders and posts a signed safe boundary", async () => {
    metadata();
    const errors = errorSpy();
    const posted: number[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    server.use(http.post(`${BASE}/api/v1/reconcile/account`, async ({ request }) => {
      const body = await request.json() as { holder: number };
      posted.push(body.holder);
      return ok({ balanced: true, gap: "0", checked_at: "2026-10-10T00:00:00Z", details: [] });
    }));
    renderPage(<Reconciliation />);
    change("Currency", "cur-1");
    for (const input of [...invalid, "0", " "]) {
      errors.mockClear();
      change("Holder", input);
      fireEvent.click(screen.getByRole("button", { name: "Check" }));
      expect(errors).toHaveBeenCalledTimes(1);
      expect(errors).toHaveBeenLastCalledWith(expect.stringMatching(/Holder/));
    }
    expect(fetchSpy.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
    change("Holder", min);
    fireEvent.click(screen.getByRole("button", { name: "Check" }));
    await waitFor(() => expect(posted).toEqual([Number.MIN_SAFE_INTEGER]));
  });

  test("template preview rejects malformed, zero and negative users before fetching", async () => {
    metadata();
    const errors = errorSpy();
    const posted: number[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    server.use(http.post(`${BASE}/api/v1/templates/settle/preview`, async ({ request }) => {
      const body = await request.json() as { holder_id: number };
      posted.push(body.holder_id);
      return ok({ entries: [] });
    }));
    renderPage(<Templates />);
    await screen.findByText("Settlement");
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    await screen.findByRole("button", { name: "Collapse" });
    for (const input of [...invalid, "0", "-42", " "]) {
      errors.mockClear();
      change("Holder ID", input);
      fireEvent.click(screen.getByRole("button", { name: "Preview" }));
      expect(errors).toHaveBeenCalledTimes(1);
      expect(errors).toHaveBeenLastCalledWith(expect.stringMatching(/Holder/));
    }
    expect(fetchSpy.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
    change("Holder ID", max);
    change("Amount", "1.00");
    fireEvent.click(skin === "shadcn" ? screen.getByRole("combobox") : screen.getByLabelText("Currency"));
    fireEvent.click(await screen.findByRole("option", { name: "USD" }));
    fireEvent.click(screen.getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(posted).toEqual([Number.MAX_SAFE_INTEGER]));
  });

  test("template journal rejects bad holder text and posts the exact positive safe boundary", async () => {
    metadata();
    const errors = errorSpy();
    const posted: number[] = [];
    const fetchSpy = vi.spyOn(globalThis, "fetch");
    server.use(http.post(`${BASE}/api/v1/journals/template`, async ({ request }) => {
      const body = await request.json() as { holder_id: number };
      posted.push(body.holder_id);
      return ok({ uid: "jr-1", entries: [] });
    }));
    renderPage(<Journals />);
    fireEvent.click(screen.getByRole("button", { name: "Template Journal" }));
    await screen.findByRole("heading", { name: "Post Template Journal" });
    for (const input of [...invalid, "0", "-42", " "]) {
      errors.mockClear();
      change("Holder ID", input);
      fireEvent.click(screen.getByRole("button", { name: "Post" }));
      expect(errors).toHaveBeenCalledTimes(1);
      expect(errors).toHaveBeenLastCalledWith(expect.stringMatching(/Holder/));
    }
    expect(fetchSpy.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
    change("Holder ID", max);
    await select("Template", "Settlement");
    await select("Currency", "USD");
    change("Amounts (JSON object)", '{"amount":"1.00"}');
    fireEvent.click(screen.getByRole("button", { name: "Post" }));
    await waitFor(() => expect(posted).toEqual([Number.MAX_SAFE_INTEGER]));
  });
});
