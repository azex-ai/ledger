import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test, vi } from "vitest";
import { toast } from "sonner";
import { DepositsPage } from "../../src/components/pages/DepositsPage";
import { WithdrawalsPage } from "../../src/components/pages/WithdrawalsPage";
import { ReservationsPage } from "../../src/components/pages/ReservationsPage";
import { renderPage, server, getOk, BASE } from "./render-page";

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

/** A failing `/classifications` lookup — the M2 trigger for every gated booking page. */
function failClassifications() {
  return http.get(`${BASE}/api/v1/classifications`, () =>
    HttpResponse.json({ code: 13000, message: { text: "boom" }, data: null }, { status: 500 }),
  );
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

  test("surfaces an error state (with Retry), not a false empty state, when /classifications fails (M2)", async () => {
    server.use(failClassifications());
    renderPage(<DepositsPage />);
    await waitFor(() =>
      expect(screen.getByText("Failed to load deposits")).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText(/no deposits/i)).not.toBeInTheDocument();
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

  // C1 regression (2026-08-26 web audit): a rejected transition used to be
  // completely silent in this skin — the AlertDialog closed, isPending
  // reverted, and nothing told the operator it failed. Pin the failure
  // feedback so this class of bug can't come back.
  test("a rejected approve shows failure feedback instead of silently succeeding", async () => {
    const errorSpy = vi.spyOn(toast, "error").mockImplementation(() => "" as never);
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [booking({ uid: "bk-9", status: "reviewing" })], next_cursor: "" }),
      http.post(`${BASE}/api/v1/bookings/bk-9/transition`, () =>
        HttpResponse.json(
          { code: 19999, message: { text: "internal error" }, data: null },
          { status: 500 },
        ),
      ),
    );
    renderPage(<WithdrawalsPage />);
    await waitFor(() => expect(screen.getByText("#bk-9")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    const dialogHeading = await screen.findByText("Approve Withdrawal #bk-9?");
    const dialog = dialogHeading.closest('[role="alertdialog"]') as HTMLElement;
    fireEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    // M1: the toast now surfaces the server's error text (message.text), not a
    // generic "Failed to …" fallback. See src/lib/error-message.ts.
    await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("internal error"));
    // The row must still show up — the transition never happened server-side.
    expect(screen.getByText("#bk-9")).toBeInTheDocument();
    errorSpy.mockRestore();
  });

  test("surfaces an error state (with Retry), not a false empty state, when /classifications fails (M2)", async () => {
    server.use(failClassifications());
    renderPage(<WithdrawalsPage />);
    await waitFor(() =>
      expect(screen.getByText("Failed to load withdrawals")).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: "Retry" })).toBeInTheDocument();
    expect(screen.queryByText(/no withdrawals/i)).not.toBeInTheDocument();
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
