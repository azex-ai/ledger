"use client";

import { useState } from "react";
import { Menu, X } from "lucide-react";
import { cn } from "../lib/utils/cn";
import {
  LEDGER_NAV_ITEMS,
  DefaultLink,
  type LinkComponent,
} from "./nav";

export interface SidebarProps {
  /** Current route, supplied by the host's router. */
  pathname: string;
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so
   * the sidebar works without a host router.
   */
  linkComponent?: LinkComponent;
}

function NavContent({
  pathname,
  linkComponent: Link = DefaultLink,
  onNavigate,
}: SidebarProps & { onNavigate?: () => void }) {
  return (
    <nav className="flex-1 overflow-y-auto p-2 space-y-0.5">
      {LEDGER_NAV_ITEMS.map((item, i) => {
        if (item.type === "separator") {
          return (
            <div key={`sep-${i}`} className="pt-4 pb-1 px-3">
              <span className="text-[10px] font-semibold uppercase tracking-widest text-muted-foreground/70">
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
              "flex items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors",
              active
                ? "bg-accent text-accent-foreground font-medium border-l-2 border-primary"
                : "text-muted-foreground hover:bg-accent/50 hover:text-foreground",
            )}
          >
            <Icon className="h-4 w-4 shrink-0" />
            {item.label}
          </Link>
        );
      })}
    </nav>
  );
}

export function Sidebar({ pathname, linkComponent }: SidebarProps) {
  const [mobileOpen, setMobileOpen] = useState(false);

  return (
    <>
      {/* Mobile top bar */}
      <div className="lg:hidden fixed top-0 left-0 right-0 z-40 flex items-center gap-3 border-b border-border bg-card px-4 py-3">
        <button
          onClick={() => setMobileOpen(true)}
          className="rounded-md p-1.5 text-muted-foreground hover:bg-accent hover:text-foreground"
          aria-label="Open navigation"
        >
          <Menu className="h-5 w-5" />
        </button>
        <div className="flex items-center gap-2">
          <div className="flex h-6 w-6 items-center justify-center rounded-md bg-primary text-primary-foreground text-xs font-bold">
            L
          </div>
          <h1 className="text-sm font-semibold tracking-tight">Ledger</h1>
        </div>
      </div>

      {/* Mobile overlay */}
      {mobileOpen && (
        <div className="lg:hidden fixed inset-0 z-50 flex">
          <div
            className="fixed inset-0 bg-black/50"
            onClick={() => setMobileOpen(false)}
          />
          <aside className="relative z-50 w-64 bg-card border-r border-border flex flex-col">
            <div className="flex items-center justify-between p-4 border-b border-border">
              <div className="flex items-center gap-2.5">
                <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary text-primary-foreground text-sm font-bold">
                  L
                </div>
                <div>
                  <h1 className="text-sm font-semibold tracking-tight">Ledger</h1>
                  <p className="text-[11px] text-muted-foreground">Admin Dashboard</p>
                </div>
              </div>
              <button
                onClick={() => setMobileOpen(false)}
                className="rounded-md p-1.5 text-muted-foreground hover:bg-accent hover:text-foreground"
                aria-label="Close navigation"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
            <NavContent
              pathname={pathname}
              linkComponent={linkComponent}
              onNavigate={() => setMobileOpen(false)}
            />
            <div className="border-t border-border p-3">
              <p className="text-[10px] text-muted-foreground/50 text-center">
                Double-Entry Ledger Engine
              </p>
            </div>
          </aside>
        </div>
      )}

      {/* Desktop sidebar */}
      <aside className="hidden lg:flex w-56 shrink-0 border-r border-border bg-card flex-col">
        <div className="p-4 border-b border-border">
          <div className="flex items-center gap-2.5">
            <div className="flex h-8 w-8 items-center justify-center rounded-lg bg-primary text-primary-foreground text-sm font-bold">
              L
            </div>
            <div>
              <h1 className="text-sm font-semibold tracking-tight">Ledger</h1>
              <p className="text-[11px] text-muted-foreground">Admin Dashboard</p>
            </div>
          </div>
        </div>
        <NavContent pathname={pathname} linkComponent={linkComponent} />
        <div className="border-t border-border p-3">
          <p className="text-[10px] text-muted-foreground/50 text-center">
            Double-Entry Ledger Engine
          </p>
        </div>
      </aside>
    </>
  );
}
