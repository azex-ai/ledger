"use client";

import { usePathname } from "next/navigation";
import { Sidebar } from "@azex/ledger-react";
import { NextLink } from "./next-link";

/**
 * Client wrapper that feeds the Next router's current pathname + a
 * `next/link` adapter into the package's router-agnostic <Sidebar>.
 */
export function AppSidebar() {
  const pathname = usePathname();
  return <Sidebar pathname={pathname} linkComponent={NextLink} />;
}
