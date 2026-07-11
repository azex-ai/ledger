import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { SweepMonitorPage } from "../../src/components/pages/SweepMonitorPage";
import { renderPage, server, getOk } from "./render-page";

function sweepBooking(over: Partial<Record<string, unknown>> = {}) {
  return {
    uid: "sw-1",
    classification_uid: "cls-20",
    account_holder: -9000000000001,
    currency_uid: "cur-1",
    amount: "1250.5",
    settled_amount: "0",
    status: "sent",
    channel_name: "onchain",
    channel_ref: "0xdeadbeef",
    reservation_uid: "",
    journal_uid: "",
    idempotency_key: "sweep-1-USDT-42",
    metadata: {
      chain_id: "1",
      token: "USDT",
      nonce: "42",
      addresses: "0xaaa,0xbbb,0xccc",
    },
    expires_at: "2026-01-02T00:00:00Z",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

function classifications() {
  return [
    { uid: "cls-20", code: "sweep", name: "Crypto Sweep", normal_side: "credit", is_system: true, is_active: true },
  ];
}

describe("SweepMonitorPage", () => {
  test("renders heading and a sweep row once the classification resolves", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [sweepBooking()], next_cursor: "" }),
    );
    renderPage(<SweepMonitorPage />);
    expect(screen.getByRole("heading", { name: "Sweep Monitor" })).toBeInTheDocument();

    await waitFor(() => expect(screen.getByText("#sw-1")).toBeInTheDocument());
    // Chain + token pulled from metadata.
    expect(screen.getByText("1")).toBeInTheDocument();
    expect(screen.getByText("USDT")).toBeInTheDocument();
    // Address count derived from the comma-joined metadata.addresses.
    expect(screen.getByText("3")).toBeInTheDocument();
    // Amount formatted per financial-display rules (>=1000 → 1 decimal, commas).
    expect(screen.getByText("1,250.5")).toBeInTheDocument();
    // Status badge + channel_ref (sweep tx hash).
    expect(screen.getByText("sent")).toBeInTheDocument();
    expect(screen.getByText("0xdeadbeef")).toBeInTheDocument();
  });

  test("renders empty state when no sweeps exist", async () => {
    server.use(
      getOk("/api/v1/classifications", classifications()),
      getOk("/api/v1/bookings", { list: [], next_cursor: "" }),
    );
    renderPage(<SweepMonitorPage />);
    await waitFor(() =>
      expect(screen.getByText("No sweeps found")).toBeInTheDocument(),
    );
  });
});
