import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test } from "vitest";
import { ReservationsPage } from "../../src/components/pages/ReservationsPage";
import { renderPage, server, getOk, BASE } from "./render-page";

function reservation(over: Partial<Record<string, unknown>> = {}) {
  return {
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
    ...over,
  };
}

/**
 * M7 (2026-08-26 web audit): correcting a rejected partial settlement was a
 * dead end because the idempotency key was minted once per dialog-open, not
 * per submitted payload. This test drives the exact reproduction sequence
 * from the audit: submit an amount the server rejects, correct the amount,
 * resubmit — and asserts the second attempt used a *different* idempotency
 * key (the fix) rather than replaying the first one (the bug, which the
 * ledger's own three-state idempotency semantics would keep bouncing as
 * ErrConflict for as long as the dialog stayed open).
 */
describe("ReservationsPage — Settle Partial idempotency key (M7)", () => {
  test("a corrected amount after a rejection mints a fresh key and succeeds", async () => {
    const capturedKeys: string[] = [];
    let rejectFirst = true;
    server.use(
      getOk("/api/v1/reservations", [reservation()]),
      http.post(`${BASE}/api/v1/reservations/rsv-3/settle-partial`, async ({ request }) => {
        const body = (await request.json()) as { amount: string; idempotency_key: string };
        capturedKeys.push(body.idempotency_key);
        if (rejectFirst) {
          rejectFirst = false;
          return HttpResponse.json(
            { code: 14003, message: { text: "amount exceeds remaining reservation" }, data: null },
            { status: 422 },
          );
        }
        return HttpResponse.json({ code: 200, message: null, data: null });
      }),
    );

    renderPage(<ReservationsPage />);
    await waitFor(() => expect(screen.getByText("#rsv-3")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Settle Partial" }));
    const dialog = await screen.findByRole("dialog");

    fireEvent.change(within(dialog).getByLabelText("Partial Amount"), {
      target: { value: "25.00" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Settle Partial" }));

    // First attempt rejected — dialog stays open (no onSuccess fired).
    await waitFor(() => expect(capturedKeys).toHaveLength(1));
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText("Partial Amount"), {
      target: { value: "20.00" },
    });
    fireEvent.click(within(dialog).getByRole("button", { name: "Settle Partial" }));

    await waitFor(() => expect(capturedKeys).toHaveLength(2));
    // The fix: a corrected payload must not replay the first key.
    expect(capturedKeys[1]).not.toBe(capturedKeys[0]);
    // Second attempt succeeded — dialog closes.
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  test("resubmitting the identical amount reuses the same key (retry semantics preserved)", async () => {
    const capturedKeys: string[] = [];
    let failCount = 0;
    server.use(
      getOk("/api/v1/reservations", [reservation()]),
      http.post(`${BASE}/api/v1/reservations/rsv-3/settle-partial`, async ({ request }) => {
        const body = (await request.json()) as { amount: string; idempotency_key: string };
        capturedKeys.push(body.idempotency_key);
        if (failCount < 1) {
          failCount++;
          return HttpResponse.json(
            { code: 19999, message: { text: "transient error" }, data: null },
            { status: 500 },
          );
        }
        return HttpResponse.json({ code: 200, message: null, data: null });
      }),
    );

    renderPage(<ReservationsPage />);
    await waitFor(() => expect(screen.getByText("#rsv-3")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Settle Partial" }));
    const dialog = await screen.findByRole("dialog");
    fireEvent.change(within(dialog).getByLabelText("Partial Amount"), {
      target: { value: "25.00" },
    });

    fireEvent.click(within(dialog).getByRole("button", { name: "Settle Partial" }));
    await waitFor(() => expect(capturedKeys).toHaveLength(1));

    // Same amount, unchanged — a real retry of the identical submission.
    fireEvent.click(within(dialog).getByRole("button", { name: "Settle Partial" }));
    await waitFor(() => expect(capturedKeys).toHaveLength(2));

    expect(capturedKeys[1]).toBe(capturedKeys[0]);
  });
});
