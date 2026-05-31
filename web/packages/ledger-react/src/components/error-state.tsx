import { AlertCircle } from "lucide-react";

export function ErrorState({ message }: { message: string }) {
  return (
    <div className="rounded-lg border border-destructive/30 bg-destructive/5 p-8 text-center">
      <AlertCircle className="mx-auto h-8 w-8 text-destructive mb-2" />
      <p className="text-sm font-medium">{message}</p>
    </div>
  );
}
