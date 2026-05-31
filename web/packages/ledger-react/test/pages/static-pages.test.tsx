import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { BalancesPage } from "../../src/components/pages/BalancesPage";
import { SnapshotsPage } from "../../src/components/pages/SnapshotsPage";
import { ReconciliationPage } from "../../src/components/pages/ReconciliationPage";
import { DashboardPage } from "../../src/components/pages/DashboardPage";
import { renderPage, server, getOk } from "./render-page";

describe("BalancesPage", () => {
  test("renders heading + search before a holder is entered (no fetch)", () => {
    // holder defaults to 0 → useBalances/useSnapshots are disabled, so no
    // network calls fire and the page shows only its header + search control.
    renderPage(<BalancesPage />);
    expect(screen.getByRole("heading", { name: "Balances" })).toBeInTheDocument();
    expect(screen.getByPlaceholderText("Account Holder ID")).toBeInTheDocument();
  });
});

describe("SnapshotsPage", () => {
  test("renders heading + empty-query prompt before searching (no fetch)", () => {
    renderPage(<SnapshotsPage />);
    expect(screen.getByRole("heading", { name: "Snapshots" })).toBeInTheDocument();
    expect(
      screen.getByText("Enter search criteria to view snapshots"),
    ).toBeInTheDocument();
  });
});

describe("ReconciliationPage", () => {
  test("renders heading and both check cards (mutations, no fetch on mount)", () => {
    renderPage(<ReconciliationPage />);
    expect(screen.getByRole("heading", { name: "Reconciliation" })).toBeInTheDocument();
    expect(screen.getByText("Global Check")).toBeInTheDocument();
    expect(screen.getByText("Account Check")).toBeInTheDocument();
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
      getOk("/api/v1/journals", { data: [], next_cursor: "" }),
    );
    renderPage(<DashboardPage />);
    expect(screen.getByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
    await waitFor(() =>
      expect(screen.getByText("Recent Journals")).toBeInTheDocument(),
    );
  });
});
