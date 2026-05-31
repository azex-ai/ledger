// Chart widgets and chart-bearing pages are isolated to this subpath so the
// heavy `recharts` dependency stays out of the root barrel (dist/index.js) and
// consumers that don't render charts never pull it into their graph.
export { BalanceTrend } from "./components/dashboard/balance-trend";

// Chart-bearing pages: DashboardPage embeds BalanceTrend; BalancesPage renders
// its own recharts line chart. Both statically import recharts, so they ship
// from here, not the root barrel.
export { DashboardPage } from "./components/pages/DashboardPage";
export type { DashboardPageProps } from "./components/pages/DashboardPage";
export { BalancesPage } from "./components/pages/BalancesPage";
