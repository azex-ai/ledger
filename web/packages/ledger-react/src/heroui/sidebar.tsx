"use client";

import type { ReactNode } from "react";
import { cn } from "@heroui/react";
import { LEDGER_NAV_ITEMS, DefaultLink, type LinkComponent } from "../components/nav";

export interface SidebarProps {
  /** Current route, supplied by the host's router. */
  pathname: string;
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so
   * the sidebar works without a host router.
   */
  linkComponent?: LinkComponent;
  /** Optional slot rendered at the bottom of the sidebar (e.g. sign-out). */
  footer?: ReactNode;
}

/**
 * Fixed desktop sidebar. No mobile drawer this round (house-scoped cut —
 * revisit once a mobile nav pattern is picked for the skin).
 */
export function Sidebar({ pathname, linkComponent: Link = DefaultLink, footer }: SidebarProps) {
  return (
    <aside className="flex w-56 shrink-0 flex-col border-r border-border bg-surface">
      <div className="border-b border-border p-4">
        <div className="flex items-center gap-2.5">
          <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-accent-soft text-accent-soft-foreground text-sm font-bold">
            L
          </div>
          <div>
            <h1 className="text-sm font-semibold tracking-tight">Ledger</h1>
            <p className="text-[11px] text-muted">Admin Dashboard</p>
          </div>
        </div>
      </div>
      <nav className="flex-1 space-y-0.5 overflow-y-auto p-2">
        {LEDGER_NAV_ITEMS.map((item, i) => {
          if (item.type === "separator") {
            return (
              <div key={`sep-${i}`} className="px-3 pt-4 pb-1">
                <span className="text-[10px] font-semibold uppercase tracking-widest text-muted/70">
                  {item.label}
                </span>
              </div>
            );
          }
          const Icon = item.icon;
          const active =
            item.href === "/" ? pathname === "/" : pathname.startsWith(item.href);
          return (
            <Link
              key={item.href}
              href={item.href}
              className={cn(
                "flex items-center gap-2.5 rounded-lg px-3 py-2 text-sm transition-colors",
                active
                  ? "bg-accent-soft text-accent-soft-foreground font-medium"
                  : "text-muted hover:bg-surface-secondary hover:text-foreground",
              )}
            >
              <Icon className="h-4 w-4 shrink-0" aria-hidden="true" />
              {item.label}
            </Link>
          );
        })}
      </nav>
      <div className="space-y-2 border-t border-border p-3">
        {footer}
        <p className="text-center text-[10px] text-muted/50">Double-Entry Ledger Engine</p>
      </div>
    </aside>
  );
}
