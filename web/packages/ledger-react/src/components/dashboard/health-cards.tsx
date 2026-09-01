"use client";

import { useHealth } from "../../hooks/use-system";
import { Card, CardContent, CardHeader, CardTitle } from "../ui/card";
import { Badge } from "../ui/badge";
import { Activity, Clock, Lock, Scale } from "lucide-react";
import { ErrorState } from "../error-state";
import { cn } from "../../lib/utils/cn";

// Same thresholds as the HeroUI skin (src/heroui/pages/DashboardPage.tsx) —
// keep in sync, otherwise operators on the two skins get different
// operational alarms from identical data (M6, 2026-08-26 web audit).
const ROLLUP_QUEUE_WARN_THRESHOLD = 100;
const CHECKPOINT_AGE_WARN_THRESHOLD_S = 300;

interface HealthBadge {
  label: string;
  className: string;
}

export function HealthCards() {
  const { data, isLoading, isError, refetch } = useHealth();

  const rollupDepth = data?.rollup_queue_depth;
  const checkpointAge = data?.checkpoint_max_age_seconds;

  const cards: { title: string; value: string | number; icon: typeof Activity; desc: string; badge?: HealthBadge }[] = [
    {
      title: "Rollup Queue",
      value: data?.rollup_queue_depth ?? "-",
      icon: Activity,
      desc: "Pending rollups",
      badge:
        rollupDepth === undefined
          ? undefined
          : rollupDepth > ROLLUP_QUEUE_WARN_THRESHOLD
            ? { label: "Backlogged", className: "bg-amber-500/15 text-amber-400 border-amber-500/20" }
            : { label: "Normal", className: "bg-emerald-500/15 text-emerald-400 border-emerald-500/20" },
    },
    {
      title: "Checkpoint Age",
      value: data ? `${data.checkpoint_max_age_seconds}s` : "-",
      icon: Clock,
      desc: "Max age (seconds)",
      badge:
        checkpointAge === undefined
          ? undefined
          : checkpointAge > CHECKPOINT_AGE_WARN_THRESHOLD_S
            ? { label: "Stale", className: "bg-amber-500/15 text-amber-400 border-amber-500/20" }
            : { label: "Fresh", className: "bg-emerald-500/15 text-emerald-400 border-emerald-500/20" },
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
              {isLoading ? (
                <span className="inline-block h-7 w-16 animate-shimmer rounded" />
              ) : (
                <div className="flex items-center gap-2">
                  <span className="text-2xl font-bold">{String(c.value)}</span>
                  {c.badge ? (
                    <Badge variant="outline" className={cn("text-xs font-medium", c.badge.className)}>
                      {c.badge.label}
                    </Badge>
                  ) : null}
                </div>
              )}
              <p className="text-xs text-muted-foreground">{c.desc}</p>
            </CardContent>
          </Card>
        );
      })}
    </div>
  );
}
