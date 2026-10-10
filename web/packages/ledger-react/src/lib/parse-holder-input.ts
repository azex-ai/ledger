/**
 * Holder text is a whole ASCII decimal integer, optionally prefixed with "-".
 * Outer whitespace and decimal leading zeros are accepted; "+", fractions,
 * exponents, radix prefixes and partial parses are rejected before conversion.
 * This UI uses the numeric SDK wire, so the full int64 range is not supported.
 *
 * optional: blank stays undefined; 0 can be a query's disabled sentinel.
 * account: user or negative system account; blank and 0 are invalid.
 * user: positive user-side holder used by template rendering.
 * A string result is a user-facing validation error, never a parsed identity.
 */
export function parseHolderInput(
  input: string,
  mode: "optional" | "account" | "user" = "optional",
): number | undefined | string {
  const text = input.trim();
  if (!text) return mode === "optional" ? undefined : "Holder is required";
  if (!/^-?[0-9]+$/.test(text)) return "Holder ID must use decimal digits with an optional leading minus sign";
  const value = Number(text);
  if (!Number.isSafeInteger(value)) {
    return "Holder ID must be between -9007199254740991 and 9007199254740991";
  }
  if (mode === "user" && value <= 0) return "Holder ID must be positive for templates";
  if (mode === "account" && value === 0) return "Holder ID must be non-zero";
  return value === 0 ? 0 : value;
}
