"use client";

import { MutationCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { useState, type ReactNode } from "react";
import { TooltipProvider } from "@/components/ui/tooltip";
import { Toaster, toast } from "sonner";
import { ApiRequestError } from "@/lib/api";

export function Providers({ children }: { children: ReactNode }) {
  const [queryClient] = useState(
    () =>
      new QueryClient({
        defaultOptions: {
          queries: {
            staleTime: 30_000,
            retry: 1,
          },
        },
        mutationCache: new MutationCache({
          onError: (error) => {
            if (error instanceof ApiRequestError) {
              toast.error(error.apiError.message, {
                description: `Error ${error.status}: ${error.apiError.code}`,
              });
            } else {
              toast.error("An unexpected error occurred", {
                description: error.message,
              });
            }
          },
        }),
      }),
  );

  return (
    <QueryClientProvider client={queryClient}>
      <TooltipProvider>
        {children}
        <Toaster
          theme="dark"
          position="bottom-right"
          toastOptions={{
            style: {
              background: "hsl(var(--card))",
              border: "1px solid hsl(var(--border))",
              color: "hsl(var(--card-foreground))",
            },
          }}
        />
      </TooltipProvider>
    </QueryClientProvider>
  );
}
