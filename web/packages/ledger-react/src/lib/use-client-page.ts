"use client";

import { useState } from "react";

/**
 * Client-side paging for lists the API returns in full (the metadata
 * surfaces: templates, classifications, journal types, currencies,
 * snapshots). Single implementation shared by both skins — each skin brings
 * its own <PaginationBar> for the controls.
 *
 * The exposed `page` is clamped to the current page count, so when the list
 * shrinks (filter change, deactivation) the view never strands on an empty
 * out-of-range page — no effect-based reset needed.
 */
export function useClientPage<T>(items: T[], pageSize = 20) {
  const [rawPage, setPage] = useState(1);
  const pageCount = Math.max(1, Math.ceil(items.length / pageSize));
  const page = Math.min(rawPage, pageCount);
  const pageItems = items.slice((page - 1) * pageSize, page * pageSize);
  return { pageItems, page, pageCount, setPage };
}
