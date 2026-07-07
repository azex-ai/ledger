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

/**
 * "Load More" footer for server-side cursor lists (infinite queries). Wire it
 * straight to useInfiniteQuery's fields; renders nothing once the cursor is
 * exhausted. Mirrored in the HeroUI skin — keep behavior in sync.
 */
export function LoadMoreBar({
  hasNextPage,
  fetchNextPage,
  isFetchingNextPage,
}: {
  hasNextPage: boolean;
  fetchNextPage: () => void;
  isFetchingNextPage: boolean;
}) {
  if (!hasNextPage) return null;
  return (
    <div className="flex justify-center">
      <Button variant="outline" size="sm" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
        {isFetchingNextPage ? "Loading..." : "Load More"}
      </Button>
    </div>
  );
}
