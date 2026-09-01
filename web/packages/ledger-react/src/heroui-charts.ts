/*
 * HeroUI chart-bearing pages — `@azex/ledger-react/heroui/charts`.
 *
 * Isolated to this subpath (mirroring the root `./charts` barrel) so the heavy
 * `recharts` dependency stays out of `dist/heroui.js`. HeroUI hosts that only
 * render table pages import from `@azex/ledger-react/heroui` and never pull
 * recharts into their graph (N9, web audit).
 */
export { DashboardPage } from "./heroui/pages/DashboardPage";
export { BalancesPage } from "./heroui/pages/BalancesPage";
