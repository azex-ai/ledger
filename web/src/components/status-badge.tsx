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
};

export function StatusBadge({ status }: { status: string }) {
  return (
    <Badge
      variant="outline"
      className={cn("text-xs font-medium", STATUS_COLORS[status] ?? "")}
    >
      {status}
    </Badge>
  );
}
