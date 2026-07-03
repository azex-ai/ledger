import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { DepositsPage } from "../../src/components/pages/DepositsPage";
import { WithdrawalsPage } from "../../src/components/pages/WithdrawalsPage";
import { ReservationsPage } from "../../src/components/pages/ReservationsPage";
import { renderPage, server, getOk } from "./render-page";

function booking(over: Partial<Record<string, unknown>> = {}) {
  return {
    id: 1,
    classification_id: 10,
    account_holder: 1001,
    currency_id: 1,
    amount: "500.00",
    settled_amount: "0",
    status: "pending",
    channel_name: "evm",
    channel_ref: "0xabc",
    reservation_id: null,
    journal_id: null,
    idempotency_key: "k1",
    metadata: {},
    expires_at: "2026-01-02T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

// Deposit/Withdraw pages resolve a classification id by code before listing.
function classifications() {
  return [
    { id: 10, code: "deposit", name: "Deposit", normal_side: "debit", is_system: true, is_active: true },
    { id: 11, code: "withdraw", name: "Withdraw", normal_side: "credit", is_system: true, is_active: true },
  ];
}

describe("DepositsPage", () => {
  test("renders heading and a deposit row once the classification resolves", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [booking({ id: 7, status: "pending" })], next_cursor: "" }),
    );
    renderPage(<DepositsPage />);
    expect(screen.getByRole("heading", { name: "Deposits" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#7")).toBeInTheDocument());
    expect(screen.getByText("evm")).toBeInTheDocument();
  });
});

describe("WithdrawalsPage", () => {
  test("renders heading and a withdrawal row once the classification resolves", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [booking({ id: 9, status: "locked" })], next_cursor: "" }),
    );
    renderPage(<WithdrawalsPage />);
    expect(screen.getByRole("heading", { name: "Withdrawals" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#9")).toBeInTheDocument());
  });
});

describe("ReservationsPage", () => {
  test("renders heading and a reservation row", async () => {
    server.use(
      getOk("/api/v1/reservations", [
        {
          id: 3,
          account_holder: 1001,
          currency_id: 1,
          reserved_amount: "100.00",
          settled_amount: "0",
          status: "active",
          journal_id: null,
          idempotency_key: "r1",
          expires_at: "2026-01-02T00:00:00Z",
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    renderPage(<ReservationsPage />);
    expect(screen.getByRole("heading", { name: "Reservations" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#3")).toBeInTheDocument());
    // Active reservations expose Settle + Release actions.
    expect(screen.getByRole("button", { name: "Settle" })).toBeInTheDocument();
  });
});
