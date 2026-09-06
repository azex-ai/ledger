import { LEDGER_NAV_ITEMS } from "@azex/ledger-react";

/** Current host product supports deposits; the package retains its full catalogue. */
export const APP_NAV_ITEMS = LEDGER_NAV_ITEMS.filter(
  (item) => item.type === "separator" || item.href !== "/withdrawals",
);
