"use client";

import { useState } from "react";
import { useReservations, useSettleReservation, useReleaseReservation } from "@/lib/hooks/use-reservations";
import { PageHeader } from "@/components/page-header";
import { StatusBadge } from "@/components/status-badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "@/components/ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "@/components/ui/dialog";
import { Label } from "@/components/ui/label";
import {
  Select, SelectContent, SelectItem, SelectTrigger, SelectValue,
} from "@/components/ui/select";

function SettleDialog({ id }: { id: number }) {
  const [open, setOpen] = useState(false);
  const [amount, setAmount] = useState("");
  const mutation = useSettleReservation();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="outline" />}>Settle</DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Settle Reservation #{id}</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <div className="grid gap-2">
            <Label>Actual Amount</Label>
            <Input value={amount} onChange={(e) => setAmount(e.target.value)} placeholder="95.50" />
          </div>
        </div>
        <DialogFooter>
          <Button
            onClick={() => mutation.mutate({ id, actualAmount: amount }, { onSuccess: () => setOpen(false) })}
            disabled={mutation.isPending || !amount}
          >
            {mutation.isPending ? "Settling..." : "Settle"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export default function ReservationsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  const { data, isLoading, hasNextPage, fetchNextPage, isFetchingNextPage } = useReservations({
    status: statusFilter || undefined,
  });
  const reservations = data?.pages.flatMap((p) => p.data) ?? [];
  const releaseMutation = useReleaseReservation();

  return (
    <div className="space-y-6">
      <PageHeader title="Reservations" description="Balance reservations (pessimistic locks)" />

      <div className="flex gap-2">
        <Select value={statusFilter} onValueChange={(v) => setStatusFilter(v ?? "")}>
          <SelectTrigger className="w-40">
            <SelectValue placeholder="All statuses" />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value="all">All</SelectItem>
            <SelectItem value="active">Active</SelectItem>
            <SelectItem value="settling">Settling</SelectItem>
            <SelectItem value="settled">Settled</SelectItem>
            <SelectItem value="released">Released</SelectItem>
          </SelectContent>
        </Select>
      </div>

      {isLoading ? (
        <div className="space-y-2">
          {Array.from({ length: 5 }).map((_, i) => (
            <div key={i} className="h-10 animate-pulse rounded bg-muted" />
          ))}
        </div>
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead>Currency</TableHead>
                <TableHead className="text-right">Reserved</TableHead>
                <TableHead className="text-right">Settled</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {reservations.map((r) => (
                <TableRow key={r.id}>
                  <TableCell>#{r.id}</TableCell>
                  <TableCell>{r.account_holder}</TableCell>
                  <TableCell>{r.currency_id}</TableCell>
                  <TableCell className="text-right font-mono">{r.reserved_amount}</TableCell>
                  <TableCell className="text-right font-mono">{r.settled_amount ?? "-"}</TableCell>
                  <TableCell><StatusBadge status={r.status} /></TableCell>
                  <TableCell className="text-xs text-muted-foreground">
                    {new Date(r.expires_at).toLocaleString()}
                  </TableCell>
                  <TableCell>
                    {r.status === "active" && (
                      <div className="flex gap-1">
                        <SettleDialog id={r.id} />
                        <Button
                          size="sm"
                          variant="ghost"
                          onClick={() => releaseMutation.mutate(r.id)}
                          disabled={releaseMutation.isPending}
                        >
                          Release
                        </Button>
                      </div>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {hasNextPage && (
            <div className="flex justify-center">
              <Button variant="outline" size="sm" onClick={() => fetchNextPage()} disabled={isFetchingNextPage}>
                {isFetchingNextPage ? "Loading..." : "Load More"}
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  );
}
