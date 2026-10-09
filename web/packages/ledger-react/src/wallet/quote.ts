import type { WalletTransactionQuote } from "./client";

/**
 * Label overrides keyed by unit code (`source_code` / `target_code`) — the
 * product-side wording anchor for measured units and currencies, the same
 * shape `kindLabels` is for `kind` (e.g. `{ INPUT_TOKEN: "input tokens" }`).
 * Falls back to the code itself: currency codes are already user-facing, and
 * a measured unit the host never named is still a fact, not a leak.
 */
export type UnitLabels = Record<string, string>;

/**
 * The one place a quote becomes words — shared by both skins so the sentence
 * cannot drift between them. Reads as "what was priced × at what rate →
 * the recorded output":
 *
 *   describeQuote({ source_code: "INPUT_TOKEN", source_quantity: "10000", rate: "0.002", ... },
 *                 { INPUT_TOKEN: "input tokens" })
 *   → "10000 input tokens × 0.002 → 20 CREDITS"
 *
 * Quantities, rates and target amounts are rendered verbatim: they are exact
 * decimal strings the ledger recorded at charge time. The target amount may
 * have been rounded, so use that snapshot, never recompute the product
 * (`formatAmount`'s display bands are for balances).
 */
export function describeQuote(quote: WalletTransactionQuote, unitLabels?: UnitLabels): string {
  const sourceUnit = unitLabels?.[quote.source_code] ?? quote.source_code;
  const targetUnit = unitLabels?.[quote.target_code] ?? quote.target_code;
  return `${quote.source_quantity} ${sourceUnit} × ${quote.rate} → ${quote.target_amount} ${targetUnit}`;
}

/** Separate complete quote explanations, each with its own recorded output. */
export function describeQuotes(quotes: readonly WalletTransactionQuote[], unitLabels?: UnitLabels): string {
  return quotes.map((q) => describeQuote(q, unitLabels)).join("; ");
}
