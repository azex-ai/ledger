import type { ComponentType, ReactNode } from "react";
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
  type LucideIcon,
} from "lucide-react";

/**
 * Link renderer the host supplies so the package stays router-agnostic.
 * The host passes its router's Link in Phase 7; the default
 * is a plain <a> so the components work without any host router.
 */
export type LinkComponent = ComponentType<{
  href: string;
  className?: string;
  children: ReactNode;
}>;

export type LedgerNavItem =
  | { type?: undefined; href: string; label: string; icon: LucideIcon }
  | { type: "separator"; label: string };

export const LEDGER_NAV_ITEMS: readonly LedgerNavItem[] = [
  { href: "/", label: "Dashboard", icon: LayoutDashboard },
  { href: "/journals", label: "Journals", icon: BookOpen },
  { href: "/balances", label: "Balances", icon: Wallet },
  { href: "/reservations", label: "Reservations", icon: Lock },
  { href: "/deposits", label: "Deposits", icon: ArrowDownToLine },
  { href: "/withdrawals", label: "Withdrawals", icon: ArrowUpFromLine },
  { type: "separator", label: "Metadata" },
  { href: "/classifications", label: "Classifications", icon: Tags },
  { href: "/journal-types", label: "Journal Types", icon: FileType2 },
  { href: "/templates", label: "Templates", icon: FileCode2 },
  { href: "/currencies", label: "Currencies", icon: Coins },
  { type: "separator", label: "Operations" },
  { href: "/reconciliation", label: "Reconciliation", icon: Scale },
  { href: "/snapshots", label: "Snapshots", icon: Camera },
];

/** Default link renderer — plain anchor, works without a host router. */
export const DefaultLink: LinkComponent = ({ href, className, children }) => (
  <a href={href} className={className}>
    {children}
  </a>
);
