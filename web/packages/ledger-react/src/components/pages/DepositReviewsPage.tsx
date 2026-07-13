"use client";

import { useState } from "react";
import {
  useDepositReviews,
  useApproveDepositReview,
  useRejectDepositReview,
} from "../../hooks/use-deposit-reviews";
import { formatAmount, formatUTC } from "../../lib/utils";
import { shortenAddress } from "../../lib/utils/address";
import type { Booking } from "../../client/types";
import { PageHeader } from "../page-header";
import { Button } from "../ui/button";
import { Textarea } from "../ui/textarea";
import { Label } from "../ui/label";
import {
  Table, TableBody, TableCell, TableHead, TableHeader, TableRow,
} from "../ui/table";
import {
  Dialog, DialogContent, DialogHeader, DialogTitle, DialogTrigger, DialogFooter,
} from "../ui/dialog";
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
  AlertDialogTrigger,
} from "../ui/alert-dialog";
import { ShieldCheck } from "lucide-react";
import { toast } from "sonner";
import { ErrorState } from "../error-state";
import { EmptyState } from "../empty-state";
import { TableSkeleton } from "../loading-skeleton";
import { LoadMoreBar } from "../pagination-bar";

// Reasons recorded on booking.metadata.review_reason by the onchain deposit
// channel (service/onchain.go reviewReasonOverCeiling/reviewReasonReconcileMismatch).
const REVIEW_REASON_LABELS: Record<string, string> = {
  over_ceiling: "Over auto-credit ceiling",
  reconcile_mismatch: "Reconciliation mismatch",
};

function metaString(metadata: Record<string, unknown>, key: string): string | undefined {
  const v = metadata[key];
  return typeof v === "string" && v !== "" ? v : undefined;
}

function reviewReasonLabel(metadata: Record<string, unknown>): string {
  const raw = metaString(metadata, "review_reason");
  if (!raw) return "Unknown";
  return REVIEW_REASON_LABELS[raw] ?? raw;
}

function OnchainCell({ metadata }: { metadata: Record<string, unknown> }) {
  const txHash = metaString(metadata, "tx_hash");
  const chainId = metaString(metadata, "chain_id");
  if (!txHash) return <span className="text-muted-foreground">—</span>;
  return (
    <div className="flex flex-col gap-0.5 text-xs">
      <span className="font-mono" title={txHash}>{shortenAddress(txHash, 6)}</span>
      {chainId && <span className="text-muted-foreground">Chain {chainId}</span>}
    </div>
  );
}

function ApproveConfirm({ booking }: { booking: Booking }) {
  const mutation = useApproveDepositReview();
  return (
    <AlertDialog>
      <AlertDialogTrigger render={<Button size="sm" variant="outline" disabled={mutation.isPending} />}>
        {mutation.isPending ? "Approving..." : "Approve"}
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Approve deposit #{booking.uid}?</AlertDialogTitle>
          <AlertDialogDescription>
            This will approve holder {booking.account_holder}&apos;s deposit of{" "}
            {formatAmount(booking.amount)} and post it to the ledger — the holder&apos;s
            balance increases immediately. This action cannot be undone.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>Cancel</AlertDialogCancel>
          <AlertDialogAction
            onClick={() =>
              mutation.mutate(booking.uid, {
                onSuccess: () => toast.success("Deposit approved and credited"),
                onError: () => toast.error("Failed to approve deposit"),
              })
            }
          >
            Approve
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  );
}

function RejectDialog({ booking }: { booking: Booking }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useRejectDepositReview();

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger render={<Button size="sm" variant="ghost" disabled={mutation.isPending} />}>
        Reject
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Reject deposit #{booking.uid}?</DialogTitle>
        </DialogHeader>
        <div className="grid gap-4 py-4">
          <p className="text-sm text-muted-foreground">
            This will reject holder {booking.account_holder}&apos;s deposit of{" "}
            {formatAmount(booking.amount)}. No funds will be credited. This action cannot be
            undone.
          </p>
          <div className="grid gap-2">
            <Label htmlFor="review-reject-reason">Reason (required)</Label>
            <Textarea
              id="review-reject-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="Why is this deposit being rejected?"
              rows={3}
            />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={() => setOpen(false)}>Cancel</Button>
          <Button
            variant="destructive"
            onClick={() =>
              mutation.mutate(
                { uid: booking.uid, reason: reason.trim() },
                {
                  onSuccess: () => {
                    toast.success("Deposit rejected");
                    setOpen(false);
                    setReason("");
                  },
                  onError: () => toast.error("Failed to reject deposit"),
                },
              )
            }
            disabled={mutation.isPending || !reason.trim()}
            title={!reason.trim() ? "Enter a reason for the rejection" : undefined}
          >
            {mutation.isPending ? "Rejecting..." : "Reject Deposit"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

export function DepositReviewsPage() {
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useDepositReviews(20);
  const reviews = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader
        title="Deposit Reviews"
        description="Deposits held for human review before posting — approving credits the holder immediately."
      />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load deposit reviews" />
      ) : reviews.length === 0 ? (
        <EmptyState
          icon={ShieldCheck}
          title="No deposits awaiting review"
          description="Deposits routed here by the compensating controls will appear as they arrive."
        />
      ) : (
        <>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>UID</TableHead>
                <TableHead>Holder</TableHead>
                <TableHead className="text-right">Amount</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead>On-chain</TableHead>
                <TableHead className="text-right">Created</TableHead>
                <TableHead>Actions</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {reviews.map((b) => (
                <TableRow key={b.uid}>
                  <TableCell className="max-w-32">
                    <span className="block truncate font-mono text-xs" title={b.uid}>
                      #{b.uid}
                    </span>
                  </TableCell>
                  <TableCell>{b.account_holder}</TableCell>
                  <TableCell className="text-right tabular-nums">
                    {formatAmount(b.amount)}
                  </TableCell>
                  <TableCell className="text-xs">{reviewReasonLabel(b.metadata)}</TableCell>
                  <TableCell><OnchainCell metadata={b.metadata} /></TableCell>
                  <TableCell className="text-right text-xs text-muted-foreground">
                    {formatUTC(b.created_at)}
                  </TableCell>
                  <TableCell>
                    <div className="flex gap-1">
                      <ApproveConfirm booking={b} />
                      <RejectDialog booking={b} />
                    </div>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </>
      )}
    </div>
  );
}
