"use client";

import { useHealth } from "../../hooks/use-system";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Activity, Clock, Lock, Scale } from "lucide-react";
import { ErrorState } from "../error-state";

export function HealthCards() {
  const { data, isLoading, isError, refetch } = useHealth();

  const cards = [
    {
      title: "Rollup Queue",
      value: data?.rollup_queue_depth ?? "-",
      icon: Activity,
      desc: "Pending rollups",
    },
    {
      title: "Checkpoint Age",
      value: data ? `${data.checkpoint_max_age_seconds}s` : "-",
      icon: Clock,
      desc: "Max age (seconds)",
    },
    {
      title: "Active Reservations",
      value: data?.active_reservations ?? "-",
      icon: Lock,
      desc: "Currently locked",
    },
    {
      title: "Status",
      value: data?.status === "ok" ? "Healthy" : data?.status === "degraded" ? "Degraded" : data?.status ?? "-",
      icon: Scale,
      desc: "System health",
    },
  ];

  if (isError) {
    return (
      <ErrorState
        message="Unable to reach the API. Health check failed — is the backend running?"
        onRetry={refetch}
      />
    );
  }

  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {cards.map((c) => {
        const Icon = c.icon;
        return (
          <Card key={c.title}>
            <CardHeader className="flex flex-row items-center justify-between pb-2">
              <CardTitle className="text-sm font-medium text-muted-foreground">
                {c.title}
              </CardTitle>
              <Icon className="h-4 w-4 text-muted-foreground" />
            </CardHeader>
            <CardContent>
              <div className="text-2xl font-bold">
                {isLoading ? (
                  <span className="inline-block h-7 w-16 animate-shimmer rounded" />
                ) : (
                  String(c.value)
                )}
              </div>
              <p className="text-xs text-muted-foreground">{c.desc}</p>
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}
