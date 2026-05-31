import { fireEvent, screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { LedgerAdmin } from "../../src/components/LedgerAdmin";
import { renderPage, server, getOk } from "./render-page";

describe("LedgerAdmin", () => {
  test("mounts the dashboard section by default and switches to Reservations via the sidebar", async () => {
    server.use(
      getOk("/api/v1/system/health", {
        status: "ok",
        rollup_queue_depth: 0,
        checkpoint_max_age_seconds: 1,
        active_reservations: 0,
      }),
      getOk("/api/v1/system/balances", []),
      getOk("/api/v1/journals", { data: [], next_cursor: "" }),
      getOk("/api/v1/reservations", []),
    );
    renderPage(<LedgerAdmin />);

    // Default section is the dashboard. DashboardPage is lazy-loaded behind a
    // Suspense boundary (to keep recharts off the root barrel), so wait for the
    // async chunk to resolve before asserting its heading.
    expect(
      await screen.findByRole("heading", { name: "Dashboard" }),
    ).toBeInTheDocument();

    // The sidebar drives internal section switching: clicking "Reservations"
    // sets the active section without navigating.
    const reservationsLinks = screen.getAllByRole("link", { name: "Reservations" });
    fireEvent.click(reservationsLinks[0]);

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Reservations" })).toBeInTheDocument(),
    );
    expect(screen.queryByRole("heading", { name: "Dashboard" })).not.toBeInTheDocument();
  });
});
