"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import { useState } from "react";
import { cn } from "@/lib/utils";
import {
  LayoutDashboard,
  BookOpen,
  Wallet,
  Lock,
  ArrowDownToLine,
  ArrowUpFromLine,
  Tags,
  FileType2,
  FileCode2,
  Coins,
  Scale,
  Camera,
  Menu,
  X,
} from "lucide-react";

const NAV_ITEMS = [
  { href: "/", label: "Dashboard", icon: LayoutDashboard },
  { href: "/journals", label: "Journals", icon: BookOpen },
  { href: "/balances", label: "Balances", icon: Wallet },
  { href: "/reservations", label: "Reservations", icon: Lock },
  { href: "/deposits", label: "Deposits", icon: ArrowDownToLine },
  { href: "/withdrawals", label: "Withdrawals", icon: ArrowUpFromLine },
  { type: "separator" as const, label: "Metadata" },
  { href: "/classifications", label: "Classifications", icon: Tags },
  { href: "/journal-types", label: "Journal Types", icon: FileType2 },
  { href: "/templates", label: "Templates", icon: FileCode2 },
  { href: "/currencies", label: "Currencies", icon: Coins },
  { type: "separator" as const, label: "Operations" },
  { href: "/reconciliation", label: "Reconciliation", icon: Scale },
  { href: "/snapshots", label: "Snapshots", icon: Camera },
] as const;

function NavContent({ onNavigate }: { onNavigate?: () => void }) {
  const pathname = usePathname();

  return (
    <nav className="flex-1 overflow-y-auto p-2 space-y-0.5">
      {NAV_ITEMS.map((item, i) => {
        if ("type" in item) {
          return (
            <div key={i} className="pt-4 pb-1 px-3">
              <span className="text-[11px] font-medium uppercase tracking-wider text-muted-foreground">
                {item.label}
              </span>
            </div>
          );
        }
        const Icon = item.icon;
        const active = item.href === "/" ? pathname === "/" : pathname.startsWith(item.href);
        return (
          <Link
            key={item.href}
            href={item.href}
            onClick={onNavigate}
            aria-current={active ? "page" : undefined}
            className={cn(
              "flex items-center gap-2.5 rounded-md px-3 py-2 text-sm transition-colors",
              active
                ? "bg-accent text-accent-foreground font-medium"
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

export function Sidebar() {
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
        <h1 className="text-sm font-semibold tracking-tight">Ledger</h1>
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
              <div>
                <h1 className="text-lg font-semibold tracking-tight">Ledger</h1>
                <p className="text-xs text-muted-foreground">Admin Dashboard</p>
              </div>
              <button
                onClick={() => setMobileOpen(false)}
                className="rounded-md p-1.5 text-muted-foreground hover:bg-accent hover:text-foreground"
                aria-label="Close navigation"
              >
                <X className="h-4 w-4" />
              </button>
            </div>
            <NavContent onNavigate={() => setMobileOpen(false)} />
          </aside>
        </div>
      )}

      {/* Desktop sidebar */}
      <aside className="hidden lg:flex w-56 shrink-0 border-r border-border bg-card flex-col">
        <div className="p-4 border-b border-border">
          <h1 className="text-lg font-semibold tracking-tight">Ledger</h1>
          <p className="text-xs text-muted-foreground">Admin Dashboard</p>
        </div>
        <NavContent />
      </aside>
    </>
  );
}
