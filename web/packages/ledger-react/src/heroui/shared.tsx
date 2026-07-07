"use client";

/*
 * Shared presentational pieces for the HeroUI skin. Small on purpose: pages
 * compose HeroUI components directly; only cross-page primitives live here.
 * Library-first: everything visual is a HeroUI component — no hand-rolled
 * overlays/tables/badges (heroui skill §3).
 */

import type { ReactNode } from "react";
import { Alert, Chip, Skeleton } from "@heroui/react";

export function PageHeader({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="flex flex-wrap items-start justify-between gap-4">
      <div className="min-w-0">
        <h1 className="text-2xl font-semibold">{title}</h1>
        {description ? (
          <p className="text-muted mt-1 text-sm">{description}</p>
        ) : null}
      </div>
      {actions ? <div className="flex items-center gap-2">{actions}</div> : null}
    </div>
  );
}

export function ErrorState({ message }: { message: string }) {
  return (
    <Alert status="danger">
      <Alert.Indicator />
      <Alert.Content>
        <Alert.Title>{message}</Alert.Title>
      </Alert.Content>
    </Alert>
  );
}

export function EmptyState({
  icon,
  title,
  description,
}: {
  icon?: ReactNode;
  title: string;
  description?: string;
}) {
  return (
    <div className="flex flex-col items-center justify-center gap-2 py-16 text-center">
      {icon}
      <p className="text-sm font-medium">{title}</p>
      {description ? <p className="text-muted text-sm">{description}</p> : null}
    </div>
  );
}

const STATUS_COLOR: Record<string, "success" | "warning" | "danger" | "default"> = {
  confirmed: "success",
  settled: "success",
  completed: "success",
  active: "success",
  ok: "success",
  pending: "warning",
  confirming: "warning",
  reserved: "warning",
  processing: "warning",
  reviewing: "warning",
  partially_settled: "warning",
  failed: "danger",
  rejected: "danger",
  expired: "danger",
  reversed: "default",
  released: "default",
  inactive: "default",
};

/** Ledger status → HeroUI Chip. Unknown statuses render neutrally. */
export function StatusChip({ status }: { status: string }) {
  return (
    <Chip color={STATUS_COLOR[status] ?? "default"} size="sm">
      {status}
    </Chip>
  );
}

/** Table-shaped loading placeholder; row height matches Table rows (h-10). */
export function TableSkeleton({ rows = 6 }: { rows?: number }) {
  return (
    <div className="space-y-2" aria-hidden>
      {Array.from({ length: rows }, (_, i) => (
        <Skeleton key={i} className="h-10 w-full rounded-lg" />
      ))}
    </div>
  );
}
