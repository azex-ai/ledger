import { fireEvent, screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test, vi } from "vitest";
import { toast } from "sonner";
import { BalancesPage } from "../../src/components/pages/BalancesPage";
import { BalancesPage as HerouiBalancesPage } from "../../src/heroui/pages/BalancesPage";
import { SnapshotsPage } from "../../src/components/pages/SnapshotsPage";
import { SnapshotsPage as HerouiSnapshotsPage } from "../../src/heroui/pages/SnapshotsPage";
import { ReconciliationPage } from "../../src/components/pages/ReconciliationPage";
import { DashboardPage } from "../../src/components/pages/DashboardPage";
import { renderPage, server, getOk, BASE } from "./render-page";

const REQUIRED_SNAPSHOT_QUERY = ["holder", "currency_uid", "start", "end"];

describe.each([
  ["shadcn", BalancesPage],
  ["heroui", HerouiBalancesPage],
])("BalancesPage (%s)", (_skin, Page) => {
  test("renders heading + search before a holder is entered (no fetch)", () => {
    // holder defaults to 0 → useBalances/useSnapshots are disabled, so no
    // network calls fire and the page shows only its header + search control.
    renderPage(<Page />);
    expect(screen.getByRole("heading", { name: "Balances" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Account Holder ID")).toBeInTheDocument();
  });

  // J-1 (2026-09-02 web audit): the trend request omitted the server-required
  // currency_uid on every input (guaranteed 400), and the dropped isError
  // made the whole card vanish instead of surfacing that. Pin both halves:
  // the request now carries currency_uid, AND a failure renders ErrorState.
  test("fetches the balance trend with the currency_uid from the balance table, not a 400-guaranteed request", async () => {
    let snapshotSearch: URLSearchParams | undefined;
    server.use(
      getOk("/api/v1/balances/1001", [
        { currency_uid: "cur-1", classification_uid: "cls-1", balance: "100" },
      ]),
      http.get(`${BASE}/api/v1/snapshots`, ({ request }) => {
        snapshotSearch = new URL(request.url).searchParams;
        for (const param of REQUIRED_SNAPSHOT_QUERY) {
          if (!snapshotSearch.get(param)) {
            return HttpResponse.json(
              { code: 12003, message: { text: `${param} is required` }, data: null },
              { status: 400 },
            );
          }
        }
        return HttpResponse.json({ code: 200, message: null, data: { list: [] } });
      }),
    );
    renderPage(<Page />);
    fireEvent.change(screen.getByPlaceholderText("Account Holder ID"), {
      target: { value: "1001" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(snapshotSearch).toBeDefined());
    expect(snapshotSearch?.get("currency_uid")).toBe("cur-1");
    // A 200 for a request that carries every required param — this is the
    // observable proof the request was NOT the old, always-400 shape.
    expect(
      screen.queryByText(/currency_uid is required/i),
    ).not.toBeInTheDocument();
  });

  test("surfaces ErrorState (not a silently vanished card) when the trend request fails", async () => {
    server.use(
      getOk("/api/v1/balances/1002", [
        { currency_uid: "cur-1", classification_uid: "cls-1", balance: "50" },
      ]),
      http.get(`${BASE}/api/v1/snapshots`, () =>
        HttpResponse.json(
          { code: 13000, message: { text: "trend unavailable" }, data: null },
          { status: 500 },
        ),
      ),
    );
    renderPage(<Page />);
    fireEvent.change(screen.getByPlaceholderText("Account Holder ID"), {
      target: { value: "1002" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() =>
      expect(screen.getByText("Balance Trend (30 days)")).toBeInTheDocument(),
    );
    await waitFor(() =>
      expect(screen.getByText("trend unavailable")).toBeInTheDocument(),
    );
  });
});

describe.each([
  ["shadcn", SnapshotsPage],
  ["heroui", HerouiSnapshotsPage],
])("SnapshotsPage (%s)", (_skin, Page) => {
  test("renders heading + empty-query prompt before searching (no fetch)", () => {
    renderPage(<Page />);
    expect(screen.getByRole("heading", { name: "Snapshots" })).toBeInTheDocument();
    expect(
      screen.getByText("Enter search criteria to view snapshots"),
    ).toBeInTheDocument();
  });

  // J-2 (2026-09-02 web audit): searching with only currency + dates (holder
  // missing) fired zero requests yet rendered "No snapshots found",
  // indistinguishable from a real empty result. Validation must block the
  // search and the network must see nothing.
  test("blocks the search and fires no request when holder is missing", async () => {
    const errorSpy = vi.spyOn(toast, "error").mockImplementation(() => "" as never);
    let requested = false;
    server.use(
      http.get(`${BASE}/api/v1/snapshots`, () => {
        requested = true;
        return HttpResponse.json({ code: 200, message: null, data: { list: [] } });
      }),
    );
    renderPage(<Page />);
    fireEvent.change(screen.getByLabelText("Currency"), { target: { value: "1" } });
    fireEvent.change(screen.getByLabelText("Start Date"), {
      target: { value: "2026-01-01" },
    });
    fireEvent.change(screen.getByLabelText("End Date"), {
      target: { value: "2026-01-31" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(errorSpy).toHaveBeenCalled());
    expect(
      screen.queryByText("No snapshots found"),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText("Enter search criteria to view snapshots"),
    ).toBeInTheDocument();
    expect(requested).toBe(false);
  });

  test("a real search that returns zero rows renders 'No snapshots found', a real request fired", async () => {
    server.use(
      getOk("/api/v1/snapshots", [], { requiredQuery: REQUIRED_SNAPSHOT_QUERY }),
    );
    renderPage(<Page />);
    fireEvent.change(screen.getByLabelText("Holder"), { target: { value: "1001" } });
    fireEvent.change(screen.getByLabelText("Currency"), { target: { value: "1" } });
    fireEvent.change(screen.getByLabelText("Start Date"), {
      target: { value: "2026-01-01" },
    });
    fireEvent.change(screen.getByLabelText("End Date"), {
      target: { value: "2026-01-31" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() =>
      expect(screen.getByText("No snapshots found")).toBeInTheDocument(),
    );
  });
});

describe("ReconciliationPage", () => {
  test("renders heading and both check cards (mutations, no fetch on mount)", () => {
    renderPage(<ReconciliationPage />);
    expect(screen.getByRole("heading", { name: "Reconciliation" })).toBeInTheDocument();
    expect(screen.getByText("Global Check")).toBeInTheDocument();
    expect(screen.getByText("Account Check")).toBeInTheDocument();
  });

  // J-19 (2026-09-02 web audit): the inline isError branch hardcoded
  // "Reconciliation failed. Check the API logs." — both a made-up internal
  // debugging instruction (user-facing-surfaces.md) and a discard of the
  // server's actual message.text.
  test("global check surfaces the server's message.text, not a hardcoded 'check the API logs'", async () => {
    server.use(
      http.post(`${BASE}/api/v1/reconcile`, () =>
        HttpResponse.json(
          { code: 13000, message: { text: "reconciliation lock held by another job" }, data: null },
          { status: 500 },
        ),
      ),
    );
    renderPage(<ReconciliationPage />);
    fireEvent.click(screen.getByRole("button", { name: "Run Global Check" }));
    await waitFor(() =>
      expect(
        screen.getByText("reconciliation lock held by another job"),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText(/check the api logs/i)).not.toBeInTheDocument();
  });
});

describe("DashboardPage", () => {
  test("renders dashboard heading and recent-journals widget", async () => {
    server.use(
      getOk("/api/v1/system/health", {
        status: "ok",
        rollup_queue_depth: 0,
        checkpoint_max_age_seconds: 1,
        active_reservations: 0,
      }),
      getOk("/api/v1/system/balances", []),
      getOk("/api/v1/journals", { list: [], next_cursor: "" }),
    );
    renderPage(<DashboardPage />);
    expect(screen.getByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByText("Recent Journals")).toBeInTheDocument(),
    );
  });
});
