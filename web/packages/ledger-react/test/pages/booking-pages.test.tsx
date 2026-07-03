import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { DepositsPage } from "../../src/components/pages/DepositsPage";
import { WithdrawalsPage } from "../../src/components/pages/WithdrawalsPage";
import { ReservationsPage } from "../../src/components/pages/ReservationsPage";
import { renderPage, server, getOk } from "./render-page";

function booking(over: Partial<Record<string, unknown>> = {}) {
  return {
    uid: "bk-1",
    classification_uid: "cls-10",
    account_holder: 1001,
    currency_uid: "cur-1",
    amount: "500.00",
    settled_amount: "0",
    status: "pending",
    channel_name: "evm",
    channel_ref: "0xabc",
    reservation_uid: "",
    journal_uid: "",
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
    { uid: "cls-10", code: "deposit", name: "Deposit", normal_side: "debit", is_system: true, is_active: true },
    { uid: "cls-11", code: "withdraw", name: "Withdraw", normal_side: "credit", is_system: true, is_active: true },
  ];
}

describe("DepositsPage", () => {
  test("renders heading and a deposit row once the classification resolves", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [booking({ uid: "bk-7", status: "pending" })], next_cursor: "" }),
    );
    renderPage(<DepositsPage />);
    expect(screen.getByRole("heading", { name: "Deposits" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#bk-7")).toBeInTheDocument());
    expect(screen.getByText("evm")).toBeInTheDocument();
  });
});

describe("WithdrawalsPage", () => {
  test("renders heading and a withdrawal row once the classification resolves", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [booking({ uid: "bk-9", status: "locked" })], next_cursor: "" }),
    );
    renderPage(<WithdrawalsPage />);
    expect(screen.getByRole("heading", { name: "Withdrawals" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#bk-9")).toBeInTheDocument());
  });
});

describe("ReservationsPage", () => {
  test("renders heading and a reservation row", async () => {
    server.use(
      getOk("/api/v1/reservations", [
        {
          uid: "rsv-3",
          account_holder: 1001,
          currency_uid: "cur-1",
          reserved_amount: "100.00",
          settled_amount: "0",
          status: "active",
          journal_uid: "",
          idempotency_key: "r1",
          expires_at: "2026-01-02T00:00:00Z",
          created_at: "2026-01-01T00:00:00Z",
          updated_at: "2026-01-01T00:00:00Z",
        },
      ]),
    );
    renderPage(<ReservationsPage />);
    expect(screen.getByRole("heading", { name: "Reservations" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#rsv-3")).toBeInTheDocument());
    // Active reservations expose Settle + Release actions.
    expect(screen.getByRole("button", { name: "Settle" })).toBeInTheDocument();
  });
});
