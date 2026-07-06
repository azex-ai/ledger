"use client";

import { usePathname, useRouter } from "next/navigation";
import { useState } from "react";
import { LogOut } from "lucide-react";
import { Sidebar } from "@azex/ledger-react";
import { NextLink } from "./next-link";

/**
 * Client wrapper that feeds the Next router's current pathname + a
 * `next/link` adapter into the package's router-agnostic <Sidebar>, plus a
 * sign-out control that clears the dashboard session cookie.
 */
export function AppSidebar() {
  const pathname = usePathname();
  const router = useRouter();
  const [signingOut, setSigningOut] = useState(false);

  async function signOut() {
    if (signingOut) return;
    setSigningOut(true);
    try {
      await fetch("/api/session", { method: "DELETE" });
    } finally {
      setSigningOut(false);
      router.replace("/login");
      router.refresh();
    }
  }

  // Hide the app chrome interactions on the login screen (it overlays anyway).
  if (pathname === "/login") return null;

  return (
    <Sidebar
      pathname={pathname}
      linkComponent={NextLink}
      footer={
        <button
          onClick={signOut}
          disabled={signingOut}
          className="flex w-full items-center justify-center gap-2 rounded-md px-3 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground disabled:opacity-50"
        >
          <LogOut className="h-3.5 w-3.5" />
          {signingOut ? "Signing out…" : "Sign out"}
        </button>
      }
    />
  );
}
