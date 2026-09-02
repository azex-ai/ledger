import { describe, expect, test } from "vitest";
import { formatAmount, formatSignedAmount, formatCompact } from "../../src/lib/utils/display";

/**
 * Pins `formatAmount`/`formatSignedAmount` against the `financial-display`
 * skill's banding table — the single authority for money display in this
 * codebase (`~/.agents/skills/financial-display/SKILL.md`).
 *
 * Before this file, the money formatter had 78 call sites and zero tests
 * (M3, 2026-08-26 web audit). A sign-dropping regression (M1) shipped
 * unnoticed because nothing pinned this module's contract.
 */
describe("formatAmount — financial-display banding table", () => {
  test(">= 1000 -> 1 decimal, thousands separator", () => {
    expect(formatAmount("72845.34")).toBe("72,845.3");
    expect(formatAmount("1000")).toBe("1,000.0");
    expect(formatAmount("1234567.89")).toBe("1,234,567.8");
  });

  test(">= 1 -> 4 decimals", () => {
    expect(formatAmount("1.23456789")).toBe("1.2345");
    expect(formatAmount("999.999999")).toBe("999.9999");
  });

  test(">= 0.01 -> 5 decimals", () => {
    expect(formatAmount("0.012345678")).toBe("0.01234");
    expect(formatAmount("0.99999999")).toBe("0.99999");
  });

  test(">= 0.0001 -> 6 decimals", () => {
    expect(formatAmount("0.0001234567")).toBe("0.000123");
    expect(formatAmount("0.009999999")).toBe("0.009999");
  });

  test("< 0.0001 -> subscript notation", () => {
    expect(formatAmount("0.00001234")).toBe("0.0₄1234");
    // Pins actual behavior, not the module docstring's illustrative shape:
    // `significantDigits` reads a fixed 18-decimal-place representation, so
    // an input with fewer than 4 meaningful digits ("712" is 3 sig figs) is
    // right-padded with a zero from that fixed width ("7120", not "712").
    // See the note on formatAmount's own doc comment in display.ts.
    expect(formatAmount("0.000000712")).toBe("0.0₆7120");
  });

  test("zero -> \"0.00\" regardless of input precision", () => {
    expect(formatAmount("0")).toBe("0.00");
    expect(formatAmount("0.0")).toBe("0.00");
    expect(formatAmount("0.000000000000000000")).toBe("0.00");
  });

  test("truncates rather than rounds at the band boundary — 0.0099999 must never round up to 0.01000", () => {
    // 0.0099999 sits in the [0.0001, 0.01) band -> 6-decimal truncation.
    // Naive rounding would produce "0.010000" and cross into the next band.
    expect(formatAmount("0.0099999")).toBe("0.009999");
  });

  test("negative amounts keep the sign in every band", () => {
    expect(formatAmount("-72845.34")).toBe("-72,845.3");
    expect(formatAmount("-1.23456789")).toBe("-1.2345");
    expect(formatAmount("-0.012345678")).toBe("-0.01234");
    expect(formatAmount("-0.0001234567")).toBe("-0.000123");
    expect(formatAmount("-0.000000712")).toBe("-0.0₆7120"); // see subscript-padding note above
  });

  test("negative zero displays as the unsigned zero form", () => {
    expect(formatAmount("-0")).toBe("0.00");
  });

  // J-11 (2026-09-02 web audit): an absent amount and a real zero were both
  // "0.00" — indistinguishable on screen. `parseUnits("", 18)` parses to
  // `0n`, so this must be checked ahead of the zero branch.
  test("empty / whitespace-only string -> \"—\" (missing), distinct from a real zero", () => {
    expect(formatAmount("")).toBe("—");
    expect(formatAmount("   ")).toBe("—");
    expect(formatAmount("0")).not.toBe(formatAmount(""));
  });
});

describe("formatCompact — dashboard-scale K/M/B notation", () => {
  test("< 1000 falls back to formatAmount, not the docstring's stale '999'", () => {
    // M3's docstring claimed formatCompact("999") -> "999"; actual behavior
    // (falls back to formatAmount's >= 1 band, 4 decimals) is "999.0000".
    expect(formatCompact("999")).toBe("999.0000");
  });

  test("K / M / B bands", () => {
    expect(formatCompact("45678")).toBe("45.7K");
    expect(formatCompact("1234567.89")).toBe("1.23M");
    expect(formatCompact("1500000000")).toBe("1.50B");
  });

  test("negative values keep the sign in every band", () => {
    expect(formatCompact("-45678")).toBe("-45.7K");
    expect(formatCompact("-1234567.89")).toBe("-1.23M");
  });

  test("zero -> \"0\"", () => {
    expect(formatCompact("0")).toBe("0");
  });

  // J-10 (2026-09-02 web audit): the docstring said "callers must clamp
  // first" but had zero callers doing so — the function now clamps itself
  // near Number.MAX_SAFE_INTEGER before the lossy Number conversion, so an
  // absurdly large value degrades to the B band's ceiling instead of
  // overflowing into NaN/Infinity.
  test("values far beyond the Number-safe range clamp instead of overflowing", () => {
    const huge = formatCompact("999999999999999999"); // 1e18ish
    expect(huge.endsWith("B")).toBe(true);
    expect(huge).not.toMatch(/NaN|Infinity/);
  });
});

describe("formatSignedAmount — PnL / drift display (M1 regression pin)", () => {
  test("positive: text carries no sign, isPositive true", () => {
    expect(formatSignedAmount("12.5")).toEqual({
      text: "12.5000",
      isPositive: true,
      isNegative: false,
    });
  });

  test("negative: text carries its own \"-\" sign — this is the exact bug M1 fixed", () => {
    expect(formatSignedAmount("-3.2")).toEqual({
      text: "-3.2000",
      isPositive: false,
      isNegative: true,
    });
  });

  test("negative below $1 still keeps the sign (band boundary regression guard)", () => {
    expect(formatSignedAmount("-0.005")).toEqual({
      text: "-0.005000",
      isPositive: false,
      isNegative: true,
    });
  });

  test("zero: no sign, both flags false", () => {
    expect(formatSignedAmount("0")).toEqual({
      text: "0.00",
      isPositive: false,
      isNegative: false,
    });
  });

  test("caller convention: prefixing \"+\" only for isPositive reproduces financial.md's 负数红色-前缀/正数+前缀 rule", () => {
    const positive = formatSignedAmount("5.5");
    const negative = formatSignedAmount("-5.5");
    expect((positive.isPositive ? "+" : "") + positive.text).toBe("+5.5000");
    expect((negative.isPositive ? "+" : "") + negative.text).toBe("-5.5000");
  });
});
