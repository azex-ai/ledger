/**
 * Journal-entry array validator shared by both skins' JournalsPage (N13, web
 * audit — the two skins previously kept byte-identical copies). Returns the
 * parsed entries on success or a human-readable error string on the first
 * invalid field. Not zod: this is the one hand-written validator in the
 * package and adding a runtime dep to a published library needs its own call.
 */

interface RawEntry {
  account_holder?: unknown;
  currency_uid?: unknown;
  classification_uid?: unknown;
  entry_type?: unknown;
  amount?: unknown;
}

export type ValidEntry = {
  account_holder: number;
  currency_uid: string;
  classification_uid: string;
  entry_type: "debit" | "credit";
  amount: string;
};

export function validateEntries(input: unknown): ValidEntry[] | string {
  if (!Array.isArray(input)) {
    return "Entries must be a JSON array";
  }
  if (input.length === 0) {
    return "Entries array must not be empty";
  }
  const out: ValidEntry[] = [];
  for (let i = 0; i < input.length; i++) {
    const e = input[i] as RawEntry;
    if (!e || typeof e !== "object") return `Entry ${i}: must be an object`;
    if (typeof e.account_holder !== "number") return `Entry ${i}: account_holder must be a number`;
    if (typeof e.currency_uid !== "string" || e.currency_uid === "") return `Entry ${i}: currency_uid must be a non-empty string`;
    if (typeof e.classification_uid !== "string" || e.classification_uid === "") return `Entry ${i}: classification_uid must be a non-empty string`;
    if (e.entry_type !== "debit" && e.entry_type !== "credit") {
      return `Entry ${i}: entry_type must be "debit" or "credit"`;
    }
    if (typeof e.amount !== "string" || e.amount === "") {
      return `Entry ${i}: amount must be a non-empty string`;
    }
    out.push({
      account_holder: e.account_holder,
      currency_uid: e.currency_uid,
      classification_uid: e.classification_uid,
      entry_type: e.entry_type,
      amount: e.amount,
    });
  }
  return out;
}
