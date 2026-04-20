import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";

const STATUS_COLORS: Record<string, string> = {
  active: "bg-green-500/15 text-green-400 border-green-500/20",
  settled: "bg-blue-500/15 text-blue-400 border-blue-500/20",
  settling: "bg-yellow-500/15 text-yellow-400 border-yellow-500/20",
  released: "bg-zinc-500/15 text-zinc-400 border-zinc-500/20",
  pending: "bg-yellow-500/15 text-yellow-400 border-yellow-500/20",
  confirming: "bg-blue-500/15 text-blue-400 border-blue-500/20",
  confirmed: "bg-green-500/15 text-green-400 border-green-500/20",
  failed: "bg-red-500/15 text-red-400 border-red-500/20",
  expired: "bg-zinc-500/15 text-zinc-400 border-zinc-500/20",
  locked: "bg-orange-500/15 text-orange-400 border-orange-500/20",
  reserved: "bg-purple-500/15 text-purple-400 border-purple-500/20",
  reviewing: "bg-yellow-500/15 text-yellow-400 border-yellow-500/20",
  processing: "bg-blue-500/15 text-blue-400 border-blue-500/20",
  reversed: "bg-orange-500/15 text-orange-400 border-orange-500/20",
  inactive: "bg-zinc-500/15 text-zinc-400 border-zinc-500/20",
  debit: "bg-green-500/15 text-green-400 border-green-500/20",
  credit: "bg-red-500/15 text-red-400 border-red-500/20",
};

const DOT_COLORS: Record<string, string> = {
  active: "bg-green-400",
  settled: "bg-blue-400",
  settling: "bg-yellow-400",
  released: "bg-zinc-400",
  pending: "bg-yellow-400",
  confirming: "bg-blue-400",
  confirmed: "bg-green-400",
  failed: "bg-red-400",
  expired: "bg-zinc-400",
  locked: "bg-orange-400",
  reserved: "bg-purple-400",
  reviewing: "bg-yellow-400",
  processing: "bg-blue-400",
  reversed: "bg-orange-400",
  inactive: "bg-zinc-400",
  debit: "bg-green-400",
  credit: "bg-red-400",
};

// Statuses that should show an animated pulse (active/live states)
const PULSE_STATUSES = new Set(["active", "confirming", "processing", "reviewing", "settling"]);

export function StatusBadge({ status }: { status: string }) {
  return (
    <Badge
      variant="outline"
      className={cn("text-xs font-medium gap-1.5", STATUS_COLORS[status] ?? "")}
    >
      <span
        className={cn(
          "inline-block h-1.5 w-1.5 rounded-full",
          DOT_COLORS[status] ?? "bg-zinc-400",
          PULSE_STATUSES.has(status) && "animate-pulse",
        )}
        aria-hidden="true"
      />
      {status}
    </Badge>
  );
}
