"use client";

/*
 * Client-side pagination footer for lists the API returns in full (metadata
 * surfaces). HeroUI Pagination compound component; renders nothing when
 * everything fits on one page. Server-side cursor lists use "Load more".
 */

import { Pagination } from "@heroui/react";

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
      <span className="text-muted text-xs tabular-nums">
        Page {page} of {pageCount}
      </span>
      <Pagination aria-label="Table pagination">
        <Pagination.Content>
          <Pagination.Item>
            <Pagination.Previous
              isDisabled={page <= 1}
              onPress={() => onPageChange(page - 1)}
            >
              <Pagination.PreviousIcon />
              <span>Previous</span>
            </Pagination.Previous>
          </Pagination.Item>
          <Pagination.Item>
            <Pagination.Next
              isDisabled={page >= pageCount}
              onPress={() => onPageChange(page + 1)}
            >
              <span>Next</span>
              <Pagination.NextIcon />
            </Pagination.Next>
          </Pagination.Item>
        </Pagination.Content>
      </Pagination>
    </div>
  );
}

/** Slice `items` for the current 1-based page. */
export function pageSlice<T>(items: T[], page: number, pageSize: number): T[] {
  return items.slice((page - 1) * pageSize, page * pageSize);
}
