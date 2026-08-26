import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, test, vi } from "vitest";
import { useCopyToClipboard } from "../../src/lib/utils/clipboard";

/*
 * C2 (2026-08-26 web audit): "Address copied" was asserted before the
 * clipboard write was known to have succeeded. These pin the hook's
 * three-state contract directly so no call site can silently regress back
 * to an optimistic boolean.
 */

describe("useCopyToClipboard", () => {
  afterEach(() => {
    vi.restoreAllMocks();
  });

  test("a successful write resolves true and settles on 'copied'", async () => {
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: vi.fn(() => Promise.resolve()) },
      configurable: true,
    });
    const { result } = renderHook(() => useCopyToClipboard());
    let ok: boolean | undefined;
    await act(async () => {
      ok = await result.current[1]("0xabc");
    });
    expect(ok).toBe(true);
    expect(result.current[0]).toBe("copied");
  });

  test("a rejected write resolves false and settles on 'failed', never 'copied'", async () => {
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: vi.fn(() => Promise.reject(new Error("denied"))) },
      configurable: true,
    });
    const { result } = renderHook(() => useCopyToClipboard());
    let ok: boolean | undefined;
    await act(async () => {
      ok = await result.current[1]("0xabc");
    });
    expect(ok).toBe(false);
    expect(result.current[0]).toBe("failed");
  });

  test("a non-secure context (navigator.clipboard undefined) resolves false synchronously, not 'copied'", async () => {
    Object.defineProperty(navigator, "clipboard", {
      value: undefined,
      configurable: true,
    });
    const { result } = renderHook(() => useCopyToClipboard());
    let ok: boolean | undefined;
    await act(async () => {
      ok = await result.current[1]("0xabc");
    });
    expect(ok).toBe(false);
    expect(result.current[0]).toBe("failed");
  });

  test("state resets to idle after resetMs", async () => {
    vi.useFakeTimers();
    Object.defineProperty(navigator, "clipboard", {
      value: { writeText: vi.fn(() => Promise.resolve()) },
      configurable: true,
    });
    const { result } = renderHook(() => useCopyToClipboard(50));
    await act(async () => {
      await result.current[1]("0xabc");
    });
    expect(result.current[0]).toBe("copied");
    await act(async () => {
      vi.advanceTimersByTime(60);
    });
    expect(result.current[0]).toBe("idle");
    vi.useRealTimers();
  });
});
