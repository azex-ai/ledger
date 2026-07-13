import { render, fireEvent, screen, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { WalletProvider } from "../../src/wallet/provider";
import { DepositAddressCard } from "../../src/wallet/components/deposit-address-card";
import { DepositAddressCard as HerouiDepositAddressCard } from "../../src/wallet/heroui/deposit-address-card";
import { server } from "../setup";

const BASE = "http://wallet.test/api/v1";
const ADDRESS = "0x0000000000000000000000000000000000000001";

function depositAddress(overrides: Partial<Record<string, unknown>> = {}) {
  return {
    uid: "da-1",
    account_holder: 7,
    address: ADDRESS,
    created_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

function ok<T>(data: T) {
  return HttpResponse.json({ code: 200, message: "ok", data });
}

// user-facing-surfaces.md guard: CREATE2 audit-only fields and internal
// mechanics must never reach the rendered deposit-address surface.
const INTERNAL_VOCABULARY = [
  /factory/i,
  /init_hash/i,
  /chain_id/i,
  /chain\b/i,
  /sweep/i,
  /provider/i,
  /webhook/i,
  /holder/i,
];

function wrap(children: ReactNode) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return (
    <WalletProvider config={{ baseUrl: BASE, queryClient: qc }}>
      {children}
    </WalletProvider>
  );
}

beforeEach(() => {
  Object.defineProperty(navigator, "clipboard", {
    value: { writeText: vi.fn(() => Promise.resolve()) },
    configurable: true,
  });
});

describe.each([
  ["shadcn", DepositAddressCard],
  ["heroui", HerouiDepositAddressCard],
])("DepositAddressCard (%s)", (_skin, Card) => {
  test("renders the address, a QR code, and no internal vocabulary", async () => {
    server.use(http.get(`${BASE}/holder/deposit-address`, () => ok(depositAddress())));
    const { container } = render(wrap(<Card />));

    await waitFor(() =>
      expect(screen.getByText("Your deposit address")).toBeInTheDocument(),
    );
    expect(screen.getByTitle(ADDRESS)).toHaveTextContent("0x0000...0001");
    expect(container.querySelector("svg")).toBeTruthy();

    const text = container.textContent ?? "";
    for (const word of INTERNAL_VOCABULARY) {
      expect(text).not.toMatch(word);
    }
  });

  test("copying the address writes it to the clipboard", async () => {
    server.use(http.get(`${BASE}/holder/deposit-address`, () => ok(depositAddress())));
    render(wrap(<Card />));

    await waitFor(() => expect(screen.getByTitle(ADDRESS)).toBeInTheDocument());
    fireEvent.click(screen.getByRole("button", { name: "Copy address" }));

    await waitFor(() =>
      expect(navigator.clipboard.writeText).toHaveBeenCalledWith(ADDRESS),
    );
  });

  test("shows a generate CTA and issues an address on 404", async () => {
    server.use(
      http.get(`${BASE}/holder/deposit-address`, () =>
        HttpResponse.json({ code: 40400, message: "not found" }, { status: 404 }),
      ),
    );
    render(wrap(<Card />));

    await waitFor(() =>
      expect(screen.getByText("Generate an address to deposit funds.")).toBeInTheDocument(),
    );

    server.use(http.post(`${BASE}/holder/deposit-address`, () => ok(depositAddress())));
    server.use(http.get(`${BASE}/holder/deposit-address`, () => ok(depositAddress())));
    fireEvent.click(screen.getByRole("button", { name: "Generate deposit address" }));

    await waitFor(() =>
      expect(screen.getByText("Your deposit address")).toBeInTheDocument(),
    );
  });

  test("shows a sanitized error state on API failure", async () => {
    server.use(
      http.get(`${BASE}/holder/deposit-address`, () =>
        HttpResponse.json(
          { code: 19999, message: "pq: deadlock detected on journal_entries" },
          { status: 500 },
        ),
      ),
    );
    const { container } = render(wrap(<Card />));

    await waitFor(() =>
      expect(
        screen.getByText("Couldn't load your deposit address", { exact: false }),
      ).toBeInTheDocument(),
    );
    expect(container.textContent).not.toContain("deadlock");
    expect(container.textContent).not.toContain("journal_entries");
  });
});
