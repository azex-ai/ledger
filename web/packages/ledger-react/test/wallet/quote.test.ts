import { describe, expect, test } from "vitest";
import { describeQuote, describeQuoteLine, describeQuotes } from "../../src/wallet/quote";
import type { WalletTransactionQuote } from "../../src/wallet/client";

const inputTokens: WalletTransactionQuote = {
  source_code: "INPUT_TOKEN",
  source_quantity: "10000",
  rate: "0.002",
  target_code: "CREDITS",
  target_amount: "20",
};
const outputTokens: WalletTransactionQuote = {
  source_code: "OUTPUT_TOKEN",
  source_quantity: "2425",
  rate: "0.005",
  target_code: "CREDITS",
  target_amount: "12.125",
};

describe("describeQuote", () => {
  test("words a priced line with source and target labels", () => {
    expect(describeQuote(inputTokens, { INPUT_TOKEN: "input tokens", CREDITS: "credits" })).toBe(
      "10000 input tokens × 0.002 → 20 credits",
    );
  });

  test("falls back to the unit code when the host has not named it", () => {
    expect(describeQuote(inputTokens)).toBe("10000 INPUT_TOKEN × 0.002 → 20 CREDITS");
  });

  test("renders the recorded decimals verbatim — an explanation never re-rounds what it explains", () => {
    expect(
      describeQuote({
        ...inputTokens,
        source_quantity: "0.000001",
        rate: "1000.0000001",
        target_amount: "0.001000000000100000",
      }),
    ).toBe("0.000001 INPUT_TOKEN × 1000.0000001 → 0.001000000000100000 CREDITS");
  });

  test("uses the recorded rounded output instead of recomputing quantity times rate", () => {
    expect(describeQuote({
      source_code: "USDC",
      source_quantity: "1",
      rate: "1000.5",
      target_code: "CREDITS",
      target_amount: "1000",
    }, { USDC: "USD Coin", CREDITS: "credits" })).toBe(
      "1 USD Coin × 1000.5 → 1000 credits",
    );
  });

  test("joins a metered charge's lines like a bill", () => {
    expect(
      describeQuotes([inputTokens, outputTokens], {
        INPUT_TOKEN: "input tokens",
        OUTPUT_TOKEN: "output tokens",
        CREDITS: "credits",
      }),
    ).toBe("10000 input tokens × 0.002 → 20 credits; 2425 output tokens × 0.005 → 12.125 credits");
    expect(describeQuotes([])).toBe("");
  });
});

describe("describeQuoteLine", () => {
  test("quotes win when present", () => {
    expect(describeQuoteLine({ quotes: [inputTokens], quotes_omitted: false })).toBe(
      "10000 INPUT_TOKEN × 0.002 → 20 CREDITS",
    );
  });

  test("a withheld breakdown gets the neutral label, overridable by the host", () => {
    expect(describeQuoteLine({ quotes: [], quotes_omitted: true })).toBe("Breakdown unavailable");
    expect(describeQuoteLine({ quotes: [], quotes_omitted: true }, undefined, "No details")).toBe("No details");
  });

  test("no conversion is no line, including from a server that predates both fields", () => {
    expect(describeQuoteLine({ quotes: [], quotes_omitted: false })).toBeNull();
    expect(describeQuoteLine({})).toBeNull();
  });
});
