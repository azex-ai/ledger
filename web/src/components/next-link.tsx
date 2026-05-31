"use client";

import Link from "next/link";
import type { ReactNode } from "react";

/**
 * Adapter wrapping `next/link` to satisfy the package's `LinkComponent`
 * contract (`{ href, className?, children }`). Supplied to package pages /
 * the sidebar so they navigate via the Next router instead of plain anchors.
 */
export function NextLink({
  href,
  className,
  children,
}: {
  href: string;
  className?: string;
  children: ReactNode;
}) {
  return (
    <Link href={href} className={className}>
      {children}
    </Link>
  );
}
