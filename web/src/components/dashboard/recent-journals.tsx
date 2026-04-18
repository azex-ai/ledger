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
import { AlertCircle } from "lucide-react";

export function RecentJournals() {
  const { data, isLoading, isError } = useJournals(10);
  const journals = data?.pages[0]?.data ?? [];

  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm font-medium">Recent Journals</CardTitle>
      </CardHeader>
      <CardContent>
        {isLoading ? (
          <div className="space-y-2">
            {Array.from({ length: 5 }).map((_, i) => (
              <div key={i} className="h-8 animate-pulse rounded bg-muted" />
            ))}
          </div>
        ) : isError ? (
          <div className="flex items-center justify-center gap-2 py-8 text-sm text-destructive">
            <AlertCircle className="h-4 w-4" />
            Failed to load journals
          </div>
        ) : journals.length === 0 ? (
          <p className="text-sm text-muted-foreground py-8 text-center">No journals yet</p>
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
                  <TableCell>{j.source}</TableCell>
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
