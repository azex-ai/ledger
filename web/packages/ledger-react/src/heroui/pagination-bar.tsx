"use client";

/*
 * Client-side pagination footer for lists the API returns in full (metadata
 * surfaces). HeroUI Pagination compound component; renders nothing when
 * everything fits on one page. Server-side cursor lists use "Load more".
 */

import { Button, Pagination, Table } from "@heroui/react";

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

/**
 * "Load More" footer for server-side cursor lists (infinite queries). Wire it
 * straight to useInfiniteQuery's fields; renders nothing once the cursor is
 * exhausted. Mirrored in the shadcn skin — keep behavior in sync. Renders its
 * own <Table.Footer>, so place it as a direct child of <Table>.
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
    <Table.Footer className="flex justify-center">
      <Button
        variant="secondary"
        size="sm"
        isPending={isFetchingNextPage}
        onPress={() => fetchNextPage()}
      >
        {isFetchingNextPage ? "Loading..." : "Load More"}
      </Button>
    </Table.Footer>
  );
}
