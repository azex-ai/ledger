import type { Entry } from "../client/types";
import { addAmounts } from "./utils";

/** Quantities only: entries of different currencies never share a total. */
export function journalTotalsByCurrency(entries: readonly Entry[]) {
  const totals = new Map<string, { currencyUid: string; debit: string; credit: string }>();
  for (const entry of entries) {
    const row = totals.get(entry.currency_uid) ?? { currencyUid: entry.currency_uid, debit: "0", credit: "0" };
    row[entry.entry_type] = addAmounts(row[entry.entry_type], entry.amount);
    totals.set(entry.currency_uid, row);
  }
  return [...totals.values()];
}
