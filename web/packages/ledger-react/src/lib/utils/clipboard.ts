"use client";

import { useState, useCallback } from "react";

export type ClipboardState = "idle" | "copied" | "failed";

/**
 * Hook for copy-to-clipboard with three-state feedback (C2, 2026-08-26 web
 * audit): "copied" is asserted only once the write is KNOWN to have
 * succeeded — never optimistically. `navigator.clipboard` is also undefined
 * outside a secure context (plain http:// on LAN/staging) or when a
 * Permissions-Policy denies clipboard-write, so the presence check below is
 * load-bearing, not defensive dead code.
 *
 *   const [state, copy] = useCopyToClipboard();
 *   <button onClick={() => copy("0xabc...").then((ok) => toast[ok ? "success" : "error"](...))}>
 *     {state === "copied" ? "Copied!" : state === "failed" ? "Copy failed" : "Copy"}
 *   </button>
 */
export function useCopyToClipboard(
  resetMs = 2000,
): [ClipboardState, (text: string) => Promise<boolean>] {
  const [state, setState] = useState<ClipboardState>("idle");

  const copy = useCallback(
    (text: string): Promise<boolean> => {
      if (!navigator.clipboard) {
        setState("failed");
        setTimeout(() => setState("idle"), resetMs);
        return Promise.resolve(false);
      }
      return navigator.clipboard.writeText(text).then(
        () => {
          setState("copied");
          setTimeout(() => setState("idle"), resetMs);
          return true;
        },
        () => {
          setState("failed");
          setTimeout(() => setState("idle"), resetMs);
          return false;
        },
      );
    },
    [resetMs],
  );

  return [state, copy];
}
