import { describe, expect, test } from "vitest";
import { parseHolderInput } from "../src/lib/parse-holder-input";

describe("strict holder text", () => {
  test.each(["12abc", "1.5", "1e3", "0x10", "0b10", "+12", "--1", "1 2", "１２", "NaN", "Infinity"])(
    "rejects non-decimal syntax %s before parsing a prefix", (input) => {
      expect(parseHolderInput(input)).toBe("Holder ID must use decimal digits with an optional leading minus sign");
    },
  );
  test.each(["9007199254740992", "9007199254740993", "-9007199254740992", "-9007199254740993"])(
    "rejects unsafe identity %s", (input) => {
      expect(parseHolderInput(input)).toContain("between -9007199254740991 and 9007199254740991");
    },
  );
  test.each([
    [" 0012 ", 12], [" -0012\t", -12], ["-0", 0],
    ["9007199254740991", Number.MAX_SAFE_INTEGER], ["-9007199254740991", Number.MIN_SAFE_INTEGER],
  ])("preserves decimal %s exactly", (input, expected) => {
    expect(parseHolderInput(input as string)).toBe(expected);
  });
  test.each(["", " \t\n "])("keeps optional blank %j undefined", (input) => {
    expect(parseHolderInput(input)).toBeUndefined();
    expect(parseHolderInput(input, "account")).toBe("Holder is required");
    expect(parseHolderInput(input, "user")).toBe("Holder is required");
  });
  test("distinguishes a query sentinel from account and positive template holders", () => {
    expect(parseHolderInput("0")).toBe(0);
    expect(parseHolderInput("0", "account")).toBe("Holder ID must be non-zero");
    expect(parseHolderInput("-42", "account")).toBe(-42);
    expect(parseHolderInput("0", "user")).toBe("Holder ID must be positive for templates");
    expect(parseHolderInput("-42", "user")).toBe("Holder ID must be positive for templates");
    expect(parseHolderInput("42", "user")).toBe(42);
  });
});
