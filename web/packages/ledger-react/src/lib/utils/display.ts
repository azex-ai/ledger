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
 *   formatAmount("0.000000712")   → "0.0₆7120"
 *   formatAmount("0")             → "0.00"
 *
 * Note on the subscript branch: the 4 digits after the leading-zero count
 * come from `significantDigits`, which reads a fixed 18-decimal-place
 * representation rather than the value's true significant-figure count —
 * an input with fewer than 4 meaningful digits (e.g. "0.000000712", "712"
 * being 3 sig figs) is right-padded with zeros from that fixed width
 * ("7120", not "712"). Documented here, not fixed — pre-existing behavior
 * discovered while adding test coverage (M3, 2026-08-26 web audit); out of
 * that audit's scope to change.
 */
export function formatAmount(value: string): string {
  // J-11 (2026-09-02 web audit): an absent amount and a real zero amount are
  // different facts — collapsing both to "0.00" makes them indistinguishable
  // on screen. `parseUnits("", 18)` happily returns `0n`, so this check must
  // come first, before the raw === 0n branch below. "—" matches the
  // existing missing-value convention already used across these pages
  // (DepositsPage/WithdrawalsPage/SweepMonitorPage's channel_ref/settled_amount).
  if (value.trim() === "") return "—";

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
 * `text` carries its own sign (a negative value keeps its "-"). Callers may
 * prepend "+" for positive values per `financial.md` ("正数加 +，颜色区分");
 * they must never re-derive or strip the sign themselves.
 *
 *   formatSignedAmount("12.5")  → { text: "12.5000",  isPositive: true,  isNegative: false }
 *   formatSignedAmount("-3.2")  → { text: "-3.2000",  isPositive: false, isNegative: true }
 *   formatSignedAmount("0")     → { text: "0.00",     isPositive: false, isNegative: false }
 */
export function formatSignedAmount(value: string): {
  text: string;
  isPositive: boolean;
  isNegative: boolean;
} {
  // J-11: same missing-vs-zero distinction as formatAmount above.
  if (value.trim() === "") return { text: "—", isPositive: false, isNegative: false };

  let raw: bigint;
  try {
    raw = parseUnits(value, 18);
  } catch {
    return { text: value, isPositive: false, isNegative: false };
  }

  return {
    text: formatAmount(value),
    isPositive: raw > 0n,
    isNegative: raw < 0n,
  };
}

// Number.MAX_SAFE_INTEGER ≈ 9.007e15; clamp comfortably under that so the
// lossy Number conversion below never silently overflows.
const FORMAT_COMPACT_MAX = 1_000_000_000_000_000; // 1e15

/**
 * Compact notation for large numbers — dashboard-scale display only, not a
 * `financial.md`-banded precise amount (see `formatAmount` for that).
 *
 *   formatCompact("1234567.89")  → "1.23M"
 *   formatCompact("45678")       → "45.7K"
 *   formatCompact("999")         → "999.0000"   (< 1000 falls back to formatAmount)
 *   formatCompact("1500000000")  → "1.50B"
 *
 * J-10 (2026-09-02 web audit): the function itself clamps values beyond
 * `FORMAT_COMPACT_MAX` (≈1e15) to that ceiling before the lossy Number
 * conversion, rather than relying on callers to clamp first — this is a
 * previously zero-caller, zero-test export, so there is no established
 * calling convention to preserve, and "the function protects itself" is
 * safer than a docstring-only contract nothing enforces.
 */
export function formatCompact(value: string): string {
  let raw: bigint;
  try {
    raw = parseUnits(value, 18);
  } catch {
    return value;
  }
  if (raw === 0n) return "0";

  const neg = raw < 0n;
  const abs = neg ? -raw : raw;
  const capped = abs > parseUnits(String(FORMAT_COMPACT_MAX), 18)
    ? parseUnits(String(FORMAT_COMPACT_MAX), 18)
    : abs;

  // For compact notation, lossy Number conversion is acceptable (reducing to
  // 3 significant digits anyway) now that `capped` guarantees it's in range.
  const num = Number(formatUnits(capped, 18));
  const prefix = neg ? "-" : "";

  if (num >= 1_000_000_000) return prefix + (num / 1_000_000_000).toFixed(2) + "B";
  if (num >= 1_000_000) return prefix + (num / 1_000_000).toFixed(2) + "M";
  if (num >= 1_000) return prefix + (num / 1_000).toFixed(1) + "K";

  return formatAmount(value);
}
