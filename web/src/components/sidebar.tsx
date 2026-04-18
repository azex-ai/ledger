"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
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

export function Sidebar() {
  const pathname = usePathname();

  return (
    <aside className="w-56 shrink-0 border-r border-border bg-card flex flex-col">
      <div className="p-4 border-b border-border">
        <h1 className="text-lg font-semibold tracking-tight">Ledger</h1>
        <p className="text-xs text-muted-foreground">Admin Dashboard</p>
      </div>
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
    </aside>
  );
}
