/**
 * Decimal primitives — re-exports viem's battle-tested BigInt ↔ string
 * conversion, plus ledger-specific display helpers that viem doesn't have.
 *
 * Consumer code should prefer viem-style usage:
 *   import { parseUnits, formatUnits } from "@/lib/utils";
 *   const wei = parseUnits("1.5", 18);
 */

import {
  parseUnits as _parseUnits,
  formatUnits as _formatUnits,
} from "viem";

// ─── Viem re-exports (parseUnits / formatUnits / convenience) ───────

export {
  parseUnits,
  formatUnits,
  parseEther,
  formatEther,
  parseGwei,
  formatGwei,
} from "viem";

// ─── Display helpers (not in viem) ──────────────────────────────────

/**
 * Count leading zeros in the fractional part of a sub-1 BigInt value.
 * Used by subscript notation (e.g. "0.0₆712" for 0.000000712).
 *
 *   leadingZeros(parseUnits("0.000007", 18), 18) → 5
 */
export function leadingZeros(value: bigint, decimals = 18): number {
  const a = value < 0n ? -value : value;
  const divisor = 10n ** BigInt(decimals);
  const remainder = a % divisor;
  if (remainder === 0n) return decimals;

  let zeros = 0;
  let probe = divisor / 10n;
  while (probe > 0n && remainder < probe) {
    zeros++;
    probe /= 10n;
  }
  return zeros;
}

/**
 * Extract up to `n` significant digits from the fractional part of a
 * sub-1 BigInt value (skipping leading zeros).
 *
 *   significantDigits(parseUnits("0.000007123", 18), 4, 18) → "7123"
 */
export function significantDigits(
  value: bigint,
  n: number,
  decimals = 18,
): string {
  const a = value < 0n ? -value : value;
  const divisor = 10n ** BigInt(decimals);
  const remainder = a % divisor;
  const fracStr = remainder.toString().padStart(decimals, "0");
  const stripped = fracStr.replace(/^0+/, "");
  return stripped.slice(0, n);
}

// ─── Amount string arithmetic ───────────────────────────────────────

/** Add two amount strings: addAmounts("1.5", "2.3") → "3.8" */
export function addAmounts(a: string, b: string, decimals = 18): string {
  return _formatUnits(
    _parseUnits(a, decimals) + _parseUnits(b, decimals),
    decimals,
  );
}

/** Subtract: subAmounts("10", "3.5") → "6.5" */
export function subAmounts(a: string, b: string, decimals = 18): string {
  return _formatUnits(
    _parseUnits(a, decimals) - _parseUnits(b, decimals),
    decimals,
  );
}

/** Compare: gtAmount("10", "3") → true */
export function gtAmount(a: string, b: string, decimals = 18): boolean {
  return _parseUnits(a, decimals) > _parseUnits(b, decimals);
}

/** Compare: gteAmount("10", "10") → true */
export function gteAmount(a: string, b: string, decimals = 18): boolean {
  return _parseUnits(a, decimals) >= _parseUnits(b, decimals);
}

/** Check zero: isZeroAmount("0.000") → true */
export function isZeroAmount(value: string, decimals = 18): boolean {
  return _parseUnits(value, decimals) === 0n;
}
