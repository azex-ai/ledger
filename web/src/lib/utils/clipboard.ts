"use client";

import { useState, useCallback } from "react";

/**
 * Hook for copy-to-clipboard with feedback state.
 *
 *   const [copied, copy] = useCopyToClipboard();
 *   <button onClick={() => copy("0xabc...")}>
 *     {copied ? "Copied!" : "Copy"}
 *   </button>
 */
export function useCopyToClipboard(resetMs = 2000): [boolean, (text: string) => void] {
  const [copied, setCopied] = useState(false);

  const copy = useCallback(
    (text: string) => {
      navigator.clipboard.writeText(text).then(() => {
        setCopied(true);
        setTimeout(() => setCopied(false), resetMs);
      });
    },
    [resetMs],
  );

  return [copied, copy];
}
