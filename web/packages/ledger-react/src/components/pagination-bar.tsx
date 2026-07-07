"use client";

import { Button } from "./ui/button";

/**
 * Client-side pagination footer for lists the API returns in full (the
 * metadata surfaces: templates, classifications, journal types, currencies,
 * snapshots). Renders nothing when everything fits on one page. Server-side
 * cursor lists use "Load more" instead — see JournalsPage.
 */
export function PaginationBar({
  page,
  pageCount,
  onPageChange,
}: {
  page: number;
  pageCount: number;
  onPageChange: (page: number) => void;
}) {
  if (pageCount <= 1) return null;
  return (
    <div className="flex items-center justify-end gap-3 pt-2">
      <span className="text-xs text-muted-foreground tabular-nums">
        Page {page} of {pageCount}
      </span>
      <div className="flex gap-1">
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1}
          onClick={() => onPageChange(page - 1)}
        >
          Previous
        </Button>
        <Button
          variant="outline"
          size="sm"
          disabled={page >= pageCount}
          onClick={() => onPageChange(page + 1)}
        >
          Next
        </Button>
      </div>
    </div>
  );
}

/** Slice `items` for the current 1-based page. */
export function pageSlice<T>(items: T[], page: number, pageSize: number): T[] {
  return items.slice((page - 1) * pageSize, page * pageSize);
}
