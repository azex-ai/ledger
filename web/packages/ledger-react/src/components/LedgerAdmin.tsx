"use client";

import { useMemo, useState, type ReactNode } from "react";
import { Sidebar } from "./sidebar";
import { type LinkComponent } from "./nav";
import { DashboardPage } from "./pages/DashboardPage";
import { JournalsPage } from "./pages/JournalsPage";
import { JournalDetailPage } from "./pages/JournalDetailPage";
import { BalancesPage } from "./pages/BalancesPage";
import { ReservationsPage } from "./pages/ReservationsPage";
import { DepositsPage } from "./pages/DepositsPage";
import { WithdrawalsPage } from "./pages/WithdrawalsPage";
import { ClassificationsPage } from "./pages/ClassificationsPage";
import { JournalTypesPage } from "./pages/JournalTypesPage";
import { TemplatesPage } from "./pages/TemplatesPage";
import { CurrenciesPage } from "./pages/CurrenciesPage";
import { ReconciliationPage } from "./pages/ReconciliationPage";
import { SnapshotsPage } from "./pages/SnapshotsPage";

/**
 * All-in-one admin shell for hosts that don't want to wire their own routes.
 * It renders the Sidebar + a content area and switches between pages via
 * INTERNAL state (no URL). The sidebar's injectable `linkComponent` is wired to
 * set the active section instead of navigating, so the existing nav drives
 * section switching. A journal-id link (`/journals/{id}`) opens JournalDetail.
 *
 * Hosts that want URL-driven routing should import the individual `*Page`
 * components and wire them to their own router instead.
 */

const JOURNAL_DETAIL_RE = /^\/journals\/(\d+)$/;

function renderSection(pathname: string, link: LinkComponent): ReactNode {
  const detailMatch = JOURNAL_DETAIL_RE.exec(pathname);
  if (detailMatch) {
    return <JournalDetailPage id={parseInt(detailMatch[1], 10)} linkComponent={link} />;
  }
  switch (pathname) {
    case "/":
      return <DashboardPage linkComponent={link} />;
    case "/journals":
      return <JournalsPage linkComponent={link} />;
    case "/balances":
      return <BalancesPage />;
    case "/reservations":
      return <ReservationsPage />;
    case "/deposits":
      return <DepositsPage />;
    case "/withdrawals":
      return <WithdrawalsPage />;
    case "/classifications":
      return <ClassificationsPage />;
    case "/journal-types":
      return <JournalTypesPage />;
    case "/templates":
      return <TemplatesPage />;
    case "/currencies":
      return <CurrenciesPage />;
    case "/reconciliation":
      return <ReconciliationPage />;
    case "/snapshots":
      return <SnapshotsPage />;
    default:
      return <DashboardPage linkComponent={link} />;
  }
}

export function LedgerAdmin() {
  const [pathname, setPathname] = useState("/");

  // Internal router: clicking a nav/page link sets the active section instead
  // of navigating. Stable identity via useCallback so child memoization holds.
  const linkComponent = useMemo<LinkComponent>(
    () =>
      function InternalLink({ href, className, children }) {
        return (
          <a
            href={href}
            className={className}
            onClick={(e) => {
              e.preventDefault();
              setPathname(href);
            }}
          >
            {children}
          </a>
        );
      },
    [],
  );

  // Sidebar active-state uses prefix matching for non-root hrefs, so a journal
  // detail path (/journals/123) still highlights the Journals nav item.
  return (
    <div className="ledger-admin flex min-h-screen">
      <Sidebar pathname={pathname} linkComponent={linkComponent} />
      <main className="flex-1 overflow-y-auto p-6 pt-16 lg:pt-6">
        {renderSection(pathname, linkComponent)}
      </main>
    </div>
  );
}
