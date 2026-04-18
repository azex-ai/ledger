import { HealthCards } from "@/components/dashboard/health-cards";
import { BalanceTrend } from "@/components/dashboard/balance-trend";
import { RecentJournals } from "@/components/dashboard/recent-journals";

export default function DashboardPage() {
  return (
    <div className="space-y-6">
      <h2 className="text-2xl font-bold tracking-tight">Dashboard</h2>
      <HealthCards />
      <div className="grid grid-cols-1 gap-6 lg:grid-cols-2">
        <BalanceTrend />
        <RecentJournals />
      </div>
    </div>
  );
}
