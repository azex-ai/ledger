import type { PreviewResult } from "../client/types";

// Preview rows have no UID. Equal rows are valid separate postings, so retain
// them and distinguish only their occurrence within the same complete content.
export function keyedPreviewEntries(entries: PreviewResult["entries"]) {
  const occurrences = new Map<string, number>();
  return entries.map((entry) => {
    const content = JSON.stringify([
      entry.account_holder, entry.currency_uid, entry.classification_uid,
      entry.entry_type, entry.amount,
    ]);
    const occurrence = occurrences.get(content) ?? 0;
    occurrences.set(content, occurrence + 1);
    return { entry, key: JSON.stringify([content, occurrence]) };
  });
}
