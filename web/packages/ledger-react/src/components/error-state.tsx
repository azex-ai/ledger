import { AlertCircle } from "lucide-react";
import { Button } from "./ui/button";

export function ErrorState({
  message,
  onRetry,
}: {
  message: string;
  /** Re-triggers the failed fetch (typically a query's `refetch`). Omit only when there is genuinely nothing to retry. */
  onRetry?: () => void;
}) {
  return (
    <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-8 text-center">
      <AlertCircle className="mx-auto h-8 w-8 text-destructive mb-2" />
      <p className="text-sm font-medium">{message}</p>
      {onRetry && (
        <Button variant="outline" size="sm" className="mt-3" onClick={onRetry}>
          Retry
        </Button>
      )}
    </div>
  );
}
