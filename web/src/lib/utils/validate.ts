/**
 * Input validation for financial amount fields.
 *
 * Uses viem's parseUnits to verify the value is parseable as a
 * BigInt decimal, plus NUMERIC(30,18) constraint checks.
 */

import { parseUnits } from "viem";

/**
 * Validate a decimal amount string for NUMERIC(30,18).
 * Returns an error message or null if valid.
 *
 *   validateAmount("123.45")  → null
 *   validateAmount("")        → "Amount is required"
 *   validateAmount("abc")     → "Invalid amount format"
 *   validateAmount("0")       → "Amount must be greater than zero"
 */
export function validateAmount(value: string): string | null {
  if (!value) return "Amount is required";

  // Structural check (no leading zeros except "0.xxx", no whitespace)
  if (!/^(0|[1-9]\d*)(\.\d+)?$/.test(value)) return "Invalid amount format";

  const [intPart, decPart = ""] = value.split(".");

  // NUMERIC(30,18) bounds
  if (intPart.length > 12) return "Amount too large (max 12 integer digits)";
  if (decPart.length > 18) return "Too many decimal places (max 18)";

  // Must parse as valid BigInt decimal
  let parsed: bigint;
  try {
    parsed = parseUnits(value, 18);
  } catch {
    return "Invalid amount format";
  }

  if (parsed === 0n) return "Amount must be greater than zero";

  return null;
}
