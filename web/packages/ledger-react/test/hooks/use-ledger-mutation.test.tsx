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

  // M4 (2026-08-26 web audit): a fresh Idempotency-Key minted on every HTTP
  // attempt defeats the ledger's replay-receipt short-circuit (I-3) —
  // api-contract.md §9 requires the key to be generated once per operation
  // and reused across retries. Pin the lifecycle directly on the hook that
  // every mutation in the package goes through.
  test("reuses the same idempotency key across a retry after failure, mints a new one after success", async () => {
    const qc = new QueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
        {children}
      </LedgerProvider>
    );

    const keys: string[] = [];
    let shouldFail = true;
    const { result } = renderHook(
      () =>
        useLedgerMutation<string, void>((_vars, idempotencyKey) => {
          keys.push(idempotencyKey);
          if (shouldFail) return Promise.reject(new Error("boom"));
          return Promise.resolve("done");
        }, []),
      { wrapper },
    );

    // Attempt 1: fails.
    result.current.mutate(undefined);
    await waitFor(() => expect(result.current.isError).toBe(true));

    // Attempt 2: a manual retry of the SAME logical operation — same key.
    result.current.mutate(undefined);
    await waitFor(() => expect(result.current.isError).toBe(true));

    expect(keys).toHaveLength(2);
    expect(keys[0]).toBe(keys[1]);

    // Attempt 3: now succeeds.
    shouldFail = false;
    result.current.mutate(undefined);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(keys).toHaveLength(3);
    expect(keys[2]).toBe(keys[0]); // still the same attempt sequence

    // Attempt 4: a NEW distinct action after success — fresh key.
    result.current.mutate(undefined);
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(keys).toHaveLength(4);
    expect(keys[3]).not.toBe(keys[0]);
  });

  // J-13 (2026-09-02 web audit): the single `useRef<string | null>` scoped
  // the whole HOOK INSTANCE to one key — safe only by coincidence (every
  // call site happened to be a per-row component). A hook instance shared
  // across entities (the shape this test drives directly, bypassing that
  // coincidence) must give a failed attempt on entity A's payload and a
  // fresh attempt on entity B's payload independent keys — the old
  // behavior would hand B the key A's failure still owned.
  test("a failure on one payload does not poison the key for a DIFFERENT payload", async () => {
    const qc = new QueryClient();
    const wrapper = ({ children }: { children: ReactNode }) => (
      <LedgerProvider config={{ baseUrl: BASE, queryClient: qc }}>
        {children}
      </LedgerProvider>
    );

    const keysByVariable: Record<string, string[]> = { A: [], B: [] };
    const { result } = renderHook(
      () =>
        useLedgerMutation<string, string>((variables, idempotencyKey) => {
          keysByVariable[variables].push(idempotencyKey);
          if (variables === "A") return Promise.reject(new Error("boom"));
          return Promise.resolve("done");
        }, []),
      { wrapper },
    );

    // A fails.
    result.current.mutate("A");
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(keysByVariable.A).toHaveLength(1);

    // B — a DIFFERENT payload — must get its own key, not A's.
    result.current.mutate("B");
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(keysByVariable.B).toHaveLength(1);
    expect(keysByVariable.B[0]).not.toBe(keysByVariable.A[0]);

    // A retried afterward still reuses A's own original key.
    result.current.mutate("A");
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(keysByVariable.A).toHaveLength(2);
    expect(keysByVariable.A[1]).toBe(keysByVariable.A[0]);
  });
});
