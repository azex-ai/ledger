import { fireEvent, screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test } from "vitest";
import { DepositReviewsPage } from "../../src/components/pages/DepositReviewsPage";
import { renderPage, server, getOk, BASE } from "./render-page";

function reviewBooking(over: Partial<Record<string, unknown>> = {}) {
  return {
    uid: "bk-1",
    classification_uid: "cls-10",
    account_holder: 1001,
    currency_uid: "cur-1",
    amount: "72845.3",
    settled_amount: "0",
    status: "review",
    channel_name: "evm",
    channel_ref: "0xabc",
    reservation_uid: "",
    journal_uid: "",
    idempotency_key: "k1",
    metadata: {
      review_reason: "over_ceiling",
      tx_hash: "0x1111111111111111111111111111111111111111111111111111111111111111",
      chain_id: "1",
    },
    expires_at: "2026-01-02T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

/**
 * A GET /deposits/reviews handler that stops returning the booking once
 * `resolved()` is true — mirrors the real backend, where a row leaves the
 * `review` status queue (and thus this endpoint's result set) the moment
 * approve/reject settles. The mutation hooks re-fetch on settle, so a
 * static mock would incorrectly resurrect the row after removal.
 */
function statefulReviewsHandler(resolved: () => boolean) {
  return http.get(`${BASE}/api/v1/deposits/reviews`, () =>
    HttpResponse.json({
      code: 200,
      message: "ok",
      data: { list: resolved() ? [] : [reviewBooking()], next_cursor: "" },
    }),
  );
}

describe("DepositReviewsPage", () => {
  test("renders heading, formatted amount, reason label, and on-chain identity", async () => {
    server.use(getOk("/api/v1/deposits/reviews", { list: [reviewBooking()], next_cursor: "" }));
    renderPage(<DepositReviewsPage />);

    expect(screen.getByRole("heading", { name: "Deposit Reviews" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#bk-1")).toBeInTheDocument());
    expect(screen.getByText("72,845.3")).toBeInTheDocument();
    expect(screen.getByText("Over auto-credit ceiling")).toBeInTheDocument();
    expect(screen.getByText("Chain 1")).toBeInTheDocument();
  });

  test("renders empty state when the review queue is empty", async () => {
    server.use(getOk("/api/v1/deposits/reviews", { list: [], next_cursor: "" }));
    renderPage(<DepositReviewsPage />);
    await waitFor(() =>
      expect(screen.getByText("No deposits awaiting review")).toBeInTheDocument(),
    );
  });

  test("approve requires confirmation, then calls the approve endpoint and removes the row", async () => {
    let approveCalled = false;
    let approved = false;
    server.use(
      statefulReviewsHandler(() => approved),
      http.post(`${BASE}/api/v1/deposits/bk-1/review/approve`, () => {
        approveCalled = true;
        approved = true;
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { uid: "bk-1", status: "confirmed" },
        });
      }),
    );
    renderPage(<DepositReviewsPage />);
    await waitFor(() => expect(screen.getByText("#bk-1")).toBeInTheDocument());

    // Clicking the row action only opens a confirmation dialog — it must not
    // call the mutation directly (money-path friction requirement).
    fireEvent.click(screen.getByRole("button", { name: "Approve" }));
    const dialogHeading = await screen.findByText("Approve deposit #bk-1?");
    const dialog = dialogHeading.closest('[role="alertdialog"]') as HTMLElement;
    expect(within(dialog).getByText(/72,845\.3/)).toBeInTheDocument();
    expect(approveCalled).toBe(false);

    fireEvent.click(within(dialog).getByRole("button", { name: "Approve" }));

    await waitFor(() => expect(screen.queryByText("#bk-1")).not.toBeInTheDocument());
    expect(approveCalled).toBe(true);
  });

  test("reject requires a reason before the submit button is enabled, then posts it", async () => {
    let capturedBody: unknown;
    let rejected = false;
    server.use(
      statefulReviewsHandler(() => rejected),
      http.post(`${BASE}/api/v1/deposits/bk-1/review/reject`, async ({ request }) => {
        capturedBody = await request.json();
        rejected = true;
        return HttpResponse.json({
          code: 200,
          message: "ok",
          data: { uid: "bk-1", status: "failed" },
        });
      }),
    );
    renderPage(<DepositReviewsPage />);
    await waitFor(() => expect(screen.getByText("#bk-1")).toBeInTheDocument());

    fireEvent.click(screen.getByRole("button", { name: "Reject" }));
    await screen.findByText("Reject deposit #bk-1?");

    const submit = screen.getByRole("button", { name: "Reject Deposit" });
    expect(submit).toBeDisabled();

    fireEvent.change(screen.getByPlaceholderText("Why is this deposit being rejected?"), {
      target: { value: "amount mismatch" },
    });
    expect(submit).not.toBeDisabled();

    fireEvent.click(submit);

    await waitFor(() => expect(screen.queryByText("#bk-1")).not.toBeInTheDocument());
    expect(capturedBody).toEqual({ reason: "amount mismatch" });
  });
});
