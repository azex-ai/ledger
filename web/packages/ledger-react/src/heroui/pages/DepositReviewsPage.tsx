"use client";

import { useState } from "react";
import {
  AlertDialog,
  Button,
  Modal,
  Table,
  TextArea,
  TextField,
  toast,
} from "@heroui/react";
import {
  useDepositReviews,
  useApproveDepositReview,
  useRejectDepositReview,
} from "../../hooks/use-deposit-reviews";
import { formatAmount, formatUTC } from "../../lib/utils";
import { shortenAddress } from "../../lib/utils/address";
import type { Booking } from "../../client/types";
import { EmptyState, ErrorState, PageHeader, TableSkeleton } from "../shared";
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
  if (!txHash) return <span className="text-muted">—</span>;
  return (
    <div className="flex flex-col gap-0.5 text-xs">
      <span className="font-mono" title={txHash}>{shortenAddress(txHash, 6)}</span>
      {chainId && <span className="text-muted">Chain {chainId}</span>}
    </div>
  );
}

function ApproveConfirm({ booking }: { booking: Booking }) {
  const [open, setOpen] = useState(false);
  const mutation = useApproveDepositReview();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Approve
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[440px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Approve deposit #{booking.uid}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This will approve holder {booking.account_holder}&apos;s deposit of{" "}
                {formatAmount(booking.amount)} and post it to the ledger — the holder&apos;s
                balance increases immediately. This action cannot be undone.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button
                variant="tertiary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(booking.uid, {
                    onSuccess: () => {
                      toast.success("Deposit approved and credited");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to approve deposit"),
                  })
                }
              >
                Approve
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function RejectModal({ booking }: { booking: Booking }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useRejectDepositReview();

  return (
    <>
      <Button size="sm" variant="ghost" onPress={() => setOpen(true)}>
        Reject
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[440px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Reject deposit #{booking.uid}?</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <p className="text-muted text-sm">
                This will reject holder {booking.account_holder}&apos;s deposit of{" "}
                {formatAmount(booking.amount)}. No funds will be credited. This action cannot
                be undone.
              </p>
              <TextField
                autoFocus
                isRequired
                aria-label="Reason"
                value={reason}
                onChange={setReason}
              >
                <TextArea rows={3} placeholder="Why is this deposit being rejected?" />
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <Button
                variant="secondary"
                isDisabled={mutation.isPending}
                onPress={() => setOpen(false)}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                isDisabled={!reason.trim()}
                onPress={() =>
                  mutation.mutate(
                    { uid: booking.uid, reason: reason.trim() },
                    {
                      onSuccess: () => {
                        toast.success("Deposit rejected");
                        setOpen(false);
                        setReason("");
                      },
                      onError: () => toast.danger("Failed to reject deposit"),
                    },
                  )
                }
              >
                Reject Deposit
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
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
          title="No deposits awaiting review"
          description="Deposits routed here by the compensating controls will appear as they arrive."
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Deposit reviews" className="min-w-[900px]">
              <Table.Header>
                <Table.Column isRowHeader>UID</Table.Column>
                <Table.Column>Holder</Table.Column>
                <Table.Column className="text-end">Amount</Table.Column>
                <Table.Column>Reason</Table.Column>
                <Table.Column>On-chain</Table.Column>
                <Table.Column className="text-end">Created</Table.Column>
                <Table.Column>Actions</Table.Column>
              </Table.Header>
              <Table.Body items={reviews}>
                {(b) => (
                  <Table.Row id={b.uid}>
                    <Table.Cell className="max-w-32">
                      <span className="block truncate font-mono text-xs" title={b.uid}>
                        #{b.uid}
                      </span>
                    </Table.Cell>
                    <Table.Cell>{b.account_holder}</Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {formatAmount(b.amount)}
                    </Table.Cell>
                    <Table.Cell className="text-xs">{reviewReasonLabel(b.metadata)}</Table.Cell>
                    <Table.Cell><OnchainCell metadata={b.metadata} /></Table.Cell>
                    <Table.Cell className="text-muted text-end text-xs">
                      {formatUTC(b.created_at)}
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap gap-1">
                        <ApproveConfirm booking={b} />
                        <RejectModal booking={b} />
                      </div>
                    </Table.Cell>
                  </Table.Row>
                )}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          <LoadMoreBar
            hasNextPage={hasNextPage}
            fetchNextPage={fetchNextPage}
            isFetchingNextPage={isFetchingNextPage}
          />
        </Table>
      )}
    </div>
  );
}
