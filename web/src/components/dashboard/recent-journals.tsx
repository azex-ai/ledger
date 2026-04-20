"use client";

import { useJournals } from "@/lib/hooks/use-journals";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import Link from "next/link";
import { AlertCircle, BookOpen } from "lucide-react";

export function RecentJournals() {
  const { data, isLoading, isError } = useJournals(10);
  const journals = data?.pages[0]?.data ?? [];

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle className="text-sm font-medium">Recent Journals</CardTitle>
        <Link
          href="/journals"
          className="text-xs text-muted-foreground hover:text-foreground transition-colors"
        >
          View all &rarr;
        </Link>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="space-y-2">
            {Array.from({ length: 5 }).map((_, i) => (
              <div key={i} className="h-8 animate-shimmer rounded" />
            ))}
          </div>
        ) : isError ? (
          <div className="flex flex-col items-center justify-center gap-2 py-8 text-sm text-destructive">
            <AlertCircle className="h-5 w-5" />
            <span>Failed to load journals</span>
          </div>
        ) : journals.length === 0 ? (
          <div className="flex flex-col items-center justify-center gap-2 py-8">
            <BookOpen className="h-8 w-8 text-muted-foreground/50" />
            <span className="text-sm text-muted-foreground">No journals yet</span>
          </div>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="w-16">ID</TableHead>
                <TableHead>Idempotency Key</TableHead>
                <TableHead>Source</TableHead>
                <TableHead className="text-right">Amount</TableHead>
                <TableHead className="text-right">Created</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {journals.map((j) => (
                <TableRow key={j.id}>
                  <TableCell>
                    <Link
                      href={`/journals/${j.id}`}
                      className="text-primary underline-offset-4 hover:underline"
                    >
                      #{j.id}
                    </Link>
                  </TableCell>
                  <TableCell className="font-mono text-xs max-w-[200px] truncate">
                    {j.idempotency_key}
                  </TableCell>
                  <TableCell className="text-muted-foreground">{j.source}</TableCell>
                  <TableCell className="text-right font-mono">{j.total_debit}</TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {new Date(j.created_at).toLocaleString()}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}
