import { act, renderHook } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { usePayloadIdempotencyKey } from "../../src/hooks/use-idempotency-key";

/**
 * Pins the M7 fix: the idempotency key belongs to the submitted payload, not
 * to the dialog's lifetime. Before this fix, `SettlePartialDialog` minted the
 * key once on dialog-open and reused it for every submit regardless of
 * amount — correcting a rejected amount replayed the same key against a
 * different payload, which the ledger's three-state idempotency semantics
 * reject as `ErrConflict` for as long as the dialog stayed open.
 */
describe("usePayloadIdempotencyKey", () => {
  test("same payload submitted twice reuses one key (retry semantics preserved)", () => {
    const { result } = renderHook(() => usePayloadIdempotencyKey());
    let first = "";
    let second = "";
    act(() => {
      first = result.current.keyFor("25.00");
    });
    act(() => {
      second = result.current.keyFor("25.00");
    });
    expect(first).toBe(second);
  });

  test("a different payload after a rejection mints a fresh key — this is the exact dead end M7 fixed", () => {
    const { result } = renderHook(() => usePayloadIdempotencyKey());
    let first = "";
    let corrected = "";
    act(() => {
      first = result.current.keyFor("25.00");
    });
    act(() => {
      corrected = result.current.keyFor("20.00");
    });
    expect(corrected).not.toBe(first);
  });

  test("reset() clears state so the next dialog open starts with a fresh key even for the same amount", () => {
    const { result } = renderHook(() => usePayloadIdempotencyKey());
    let first = "";
    let afterReset = "";
    act(() => {
      first = result.current.keyFor("25.00");
    });
    act(() => {
      result.current.reset();
    });
    act(() => {
      afterReset = result.current.keyFor("25.00");
    });
    expect(afterReset).not.toBe(first);
  });
});
