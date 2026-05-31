import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";
import { LedgerProvider } from "../../src/provider/provider";
import { useLedgerMutation } from "../../src/hooks/use-ledger-mutation";

const BASE = "http://ledger.test";

describe("useLedgerMutation", () => {
  test("prepends 'ledger' to caller keys and invalidates balances", async () => {
    const qc = new QueryClient();
    const spy = vi.spyOn(qc, "invalidateQueries");

    const wrapper = ({ children }: { children: ReactNode }) => (
      <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
        {children}
      </LedgerProvider>
    );

    const { result } = renderHook(
      () => useLedgerMutation(async () => "done", ["journals"]),
      { wrapper },
    );

    result.current.mutate(undefined);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));

    const invalidated = spy.mock.calls.map((c) => c[0]?.queryKey);
    expect(invalidated).toContainEqual(["ledger", "journals"]);
    expect(invalidated).toContainEqual(["ledger", "balances"]);
    expect(invalidated).toContainEqual(["ledger", "system-balances"]);
  });
});
