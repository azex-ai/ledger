// Reusable skeleton building blocks for loading.tsx files.
// Skeleton structure MUST match the corresponding page structure to prevent layout shift.

export function PageHeaderSkeleton({ hasActions = false }: { hasActions?: boolean }) {
  return (
    <div className="flex items-center justify-between">
      <div>
        <div className="h-8 w-48 animate-pulse rounded bg-muted" />
        <div className="mt-1 h-4 w-72 animate-pulse rounded bg-muted" />
      </div>
      {hasActions && <div className="h-9 w-20 animate-pulse rounded bg-muted" />}
    </div>
  );
}

export function TableSkeleton({ rows = 5, cols = 6 }: { rows?: number; cols?: number }) {
  return (
    <div className="space-y-2">
      <div className="flex gap-4 px-4 py-2">
        {Array.from({ length: cols }).map((_, i) => (
          <div key={i} className="h-4 flex-1 animate-pulse rounded bg-muted" />
        ))}
      </div>
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="h-12 animate-pulse rounded bg-muted" />
      ))}
    </div>
  );
}

export function FilterSkeleton() {
  return <div className="h-10 w-40 animate-pulse rounded bg-muted" />;
}

export function ListPageSkeleton({
  hasFilter = false,
  hasActions = false,
  rows = 5,
}: {
  hasFilter?: boolean;
  hasActions?: boolean;
  rows?: number;
}) {
  return (
    <div className="space-y-6">
      <PageHeaderSkeleton hasActions={hasActions} />
      {hasFilter && <FilterSkeleton />}
      <TableSkeleton rows={rows} />
    </div>
  );
}
