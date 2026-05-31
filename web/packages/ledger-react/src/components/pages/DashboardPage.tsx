"use client";

import { HealthCards } from "../dashboard/health-cards";
import { BalanceTrend } from "../dashboard/balance-trend";
import { RecentJournals } from "../dashboard/recent-journals";
import { DefaultLink, type LinkComponent } from "../nav";

export interface DashboardPageProps {
  /**
   * Link renderer supplied by the host's router. Defaults to a plain <a> so the
   * page works without a host router. Forwarded to RecentJournals for its
   * "View all" link.
   */
  linkComponent?: LinkComponent;
}

export function DashboardPage({ linkComponent = DefaultLink }: DashboardPageProps = {}) {
  return (
    <div className="space-y-6">
      <div>
        <h2 className="text-2xl font-bold tracking-tight">Dashboard</h2>
        <p className="text-sm text-muted-foreground">
          System overview and recent activity
        </p>
      </div>
      <HealthCards />
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <BalanceTrend />
        <RecentJournals linkComponent={linkComponent} />
      </div>
    </div>
  );
}
