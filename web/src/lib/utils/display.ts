/**
 * Financial amount display formatting.
 *
 * Magnitude detection uses native BigInt comparison on viem-parsed values.
 *
 * Display rules (from financial.md):
 *   >= 1000   → 1 decimal, thousands separator  (72,845.3)
 *   >= 1      → 4 decimals                      (1.2345)
 *   >= 0.01   → 5 decimals                      (0.01234)
 *   >= 0.0001 → 6 decimals                      (0.000123)
 *   < 0.0001  → subscript notation               (0.0₆712)
 *   zero      → "0.00"
 */

import { parseUnits, formatUnits } from "viem";
import { leadingZeros, significantDigits } from "./decimal";

// Pre-computed thresholds (BigInt, 18 decimals)
const T_1000 = parseUnits("1000", 18);
const T_1 = parseUnits("1", 18);
const T_001 = parseUnits("0.01", 18);
const T_00001 = parseUnits("0.0001", 18);

// ─── Subscript digits ───────────────────────────────────────────────

const SUB = ["₀", "₁", "₂", "₃", "₄", "₅", "₆", "₇", "₈", "₉"] as const;

function toSubscript(n: number): string {
  return String(n)
    .split("")
    .map((c) => SUB[parseInt(c, 10)])
    .join("");
}

// ─── Internal helpers ───────────────────────────────────────────────

function addCommas(s: string): string {
  return s.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/** Truncate/pad to exactly `places` fractional digits, optional commas. */
function toFixed(value: bigint, places: number, commas: boolean): string {
  const raw = formatUnits(value, 18);
  const [intPart = "0", fracRaw = ""] = raw.split(".");
  const frac =
    places > 0 ? "." + (fracRaw + "0".repeat(places)).slice(0, places) : "";
  return (commas ? addCommas(intPart) : intPart) + frac;
}

// ─── Public API ─────────────────────────────────────────────────────

/**
 * Format a decimal string for display without precision loss.
 *
 *   formatAmount("72845.3")       → "72,845.3"
 *   formatAmount("1.23456789")    → "1.2345"
 *   formatAmount("0.000000712")   → "0.0₆712"
 *   formatAmount("0")             → "0.00"
 */
export function formatAmount(value: string): string {
  let raw: bigint;
  try {
    raw = parseUnits(value, 18);
  } catch {
    return value;
  }

  if (raw === 0n) return "0.00";

  const neg = raw < 0n;
  const a = neg ? -raw : raw;
  const prefix = neg ? "-" : "";

  if (a >= T_1000) return prefix + toFixed(a, 1, true);
  if (a >= T_1) return prefix + toFixed(a, 4, false);
  if (a >= T_001) return prefix + toFixed(a, 5, false);
  if (a >= T_00001) return prefix + toFixed(a, 6, false);

  // Subscript notation for very small values
  const zeros = leadingZeros(a);
  const sig = significantDigits(a, 4);
  return `${prefix}0.0${toSubscript(zeros)}${sig}`;
}

/**
 * Format a signed amount for PnL / drift display.
 *
 *   formatSignedAmount("12.5")  → { text: "12.5000", isPositive: true,  isNegative: false }
 *   formatSignedAmount("-3.2")  → { text: "3.2000",  isPositive: false, isNegative: true }
 *   formatSignedAmount("0")     → { text: "0.00",    isPositive: false, isNegative: false }
 */
export function formatSignedAmount(value: string): {
  text: string;
  isPositive: boolean;
  isNegative: boolean;
} {
  let raw: bigint;
  try {
    raw = parseUnits(value, 18);
  } catch {
    return { text: value, isPositive: false, isNegative: false };
  }

  if (raw === 0n) {
    return { text: "0.00", isPositive: false, isNegative: false };
  }

  const a = raw < 0n ? -raw : raw;
  const formatted = formatAmount(formatUnits(a, 18));

  return {
    text: formatted,
    isPositive: raw > 0n,
    isNegative: raw < 0n,
  };
}

/**
 * Compact notation for large numbers.
 *
 *   formatCompact("1234567.89")  → "1.23M"
 *   formatCompact("45678")       → "45.7K"
 *   formatCompact("999")         → "999"
 *   formatCompact("1500000000")  → "1.50B"
 */
export function formatCompact(value: string): string {
  let raw: bigint;
  try {
    raw = parseUnits(value, 18);
  } catch {
    return value;
  }
  if (raw === 0n) return "0";

  // For compact notation, lossy Number conversion is acceptable
  // (we're reducing to 3 significant digits anyway).
  //
  // Cap: values up to ~$1e15 (Number.MAX_SAFE_INTEGER ≈ 9e15) are safe.
  // Beyond that the Number conversion overflows; callers must clamp first.
  const num = Number(formatUnits(raw < 0n ? -raw : raw, 18));
  const prefix = raw < 0n ? "-" : "";

  if (num >= 1_000_000_000) return prefix + (num / 1_000_000_000).toFixed(2) + "B";
  if (num >= 1_000_000) return prefix + (num / 1_000_000).toFixed(2) + "M";
  if (num >= 1_000) return prefix + (num / 1_000).toFixed(1) + "K";

  return formatAmount(value);
}
