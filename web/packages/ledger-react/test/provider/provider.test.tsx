import { render, renderHook, screen, waitFor } from "@testing-library/react";
import {
  QueryClient,
  useQuery,
  useQueryClient,
} from "@tanstack/react-query";
import { http, HttpResponse } from "msw";
import type { ReactNode } from "react";
import { describe, expect, test } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import { useLedgerClient } from "../../src/provider/context";
import { server } from "../setup";

const BASE = "http://ledger.test";

describe("LedgerProvider", () => {
  test("(a) provides a working client that hits MSW", async () => {
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () =>
        HttpResponse.json({ code: 0, message: "ok", data: { status: "ok" } }),
      ),
    );
    const wrapper = ({ children }: { children: ReactNode }) => (
      <LedgerProvider config={{ baseUrl: BASE }}>{children}</LedgerProvider>
    );
    const { result } = renderHook(() => useLedgerClient(), { wrapper });
    await expect(result.current.getHealth()).resolves.toEqual({
      status: "ok",
    });
  });

  test("(b) creates its own QueryClient so useQuery works", async () => {
    function Probe() {
      const q = useQuery({ queryKey: ["k"], queryFn: () => "value" });
      return <div data-testid="q">{q.data ?? "loading"}</div>;
    }
    render(
      <LedgerProvider config={{ baseUrl: BASE }}>
        <Probe />
      </LedgerProvider>,
    );
    await waitFor(() =>
      expect(screen.getByTestId("q")).toHaveTextContent("value"),
    );
  });

  test("(c) reuses an injected QueryClient (identity)", () => {
    const injected = new QueryClient();
    let seen: QueryClient | undefined;
    function Probe() {
      seen = useQueryClient();
      return null;
    }
    render(
      <LedgerProvider config={{ baseUrl: BASE, queryClient: injected }}>
        <Probe />
      </LedgerProvider>,
    );
    expect(seen).toBe(injected);
  });

  test("(d) renders a .ledger-root wrapper", () => {
    const { container } = render(
      <LedgerProvider config={{ baseUrl: BASE }}>
        <span>child</span>
      </LedgerProvider>,
    );
    expect(container.querySelector(".ledger-root")).not.toBeNull();
  });

  test("(e) applies theme CSS vars to .ledger-root", () => {
    const { container } = render(
      <LedgerProvider config={{ baseUrl: BASE, theme: { "--primary": "red" } }}>
        <span>child</span>
      </LedgerProvider>,
    );
    const root = container.querySelector(".ledger-root") as HTMLElement;
    expect(root.style.getPropertyValue("--primary")).toBe("red");
  });

  test("(f) keeps a stable client identity across re-renders", () => {
    const seen: unknown[] = [];
    function Probe() {
      seen.push(useLedgerClient());
      return null;
    }
    const { rerender } = render(
      <LedgerProvider config={{ baseUrl: BASE }}>
        <Probe />
      </LedgerProvider>,
    );
    rerender(
      <LedgerProvider config={{ baseUrl: BASE }}>
        <Probe />
      </LedgerProvider>,
    );
    expect(seen.length).toBeGreaterThanOrEqual(2);
    expect(seen[1]).toBe(seen[0]);
  });
});
