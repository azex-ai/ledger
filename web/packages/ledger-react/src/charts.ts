// Chart widgets are isolated to this subpath so the heavy `recharts`
// dependency stays out of the root barrel and consumers that don't render
// charts never pull it into their graph.
export { BalanceTrend } from "./components/dashboard/balance-trend";
