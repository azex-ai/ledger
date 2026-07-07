"use client";

import { useMemo, useState } from "react";
import {
  AlertDialog,
  Button,
  Input,
  Label,
  ListBox,
  Modal,
  Select,
  Table,
  TextField,
  toast,
} from "@heroui/react";
import {
  useConfirmWithdraw,
  useFailWithdraw,
  useProcessWithdraw,
  useRetryWithdraw,
  useReserveWithdraw,
  useReviewWithdraw,
  useWithdrawals,
} from "../../hooks/use-withdrawals";
import { formatAmount, formatUTC } from "../../lib/utils";
import { EmptyState, ErrorState, PageHeader, StatusChip, TableSkeleton } from "../shared";

const WITHDRAW_STATES = [
  "locked",
  "reserved",
  "reviewing",
  "processing",
  "confirmed",
  "failed",
  "expired",
];

function ProcessModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [channelRef, setChannelRef] = useState("");
  const mutation = useProcessWithdraw();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Process
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Process Withdrawal #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body>
              <TextField autoFocus aria-label="Channel Ref" value={channelRef} onChange={setChannelRef}>
                <Input placeholder="0xdef..." />
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
                isPending={mutation.isPending}
                isDisabled={!channelRef}
                onPress={() =>
                  mutation.mutate(
                    { id, channelRef },
                    {
                      onSuccess: () => {
                        toast.success("Withdrawal processing");
                        setOpen(false);
                        setChannelRef("");
                      },
                      onError: () => toast.danger("Failed to process withdrawal"),
                    },
                  )
                }
              >
                Submit
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function FailModal({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const [reason, setReason] = useState("");
  const mutation = useFailWithdraw();

  return (
    <>
      <Button size="sm" variant="ghost" onPress={() => setOpen(true)}>
        Fail
      </Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-[400px]">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Fail Withdrawal #{id}</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <p className="text-muted text-sm">
                This will mark the withdrawal as failed. You can retry from the failed state.
              </p>
              <TextField autoFocus aria-label="Reason" value={reason} onChange={setReason}>
                <Input placeholder="Insufficient gas, timeout, etc." />
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
                onPress={() =>
                  mutation.mutate(
                    { id, reason: reason || "manual fail" },
                    {
                      onSuccess: () => {
                        toast.success("Withdrawal marked as failed");
                        setOpen(false);
                        setReason("");
                      },
                      onError: () => toast.danger("Failed to update withdrawal"),
                    },
                  )
                }
              >
                Fail Withdrawal
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function ReserveConfirm({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useReserveWithdraw();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Reserve
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Reserve Withdrawal #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This will lock funds for this withdrawal. The reserved amount will be deducted
                from available balance.
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
                  mutation.mutate(id, {
                    onSuccess: () => {
                      toast.success("Withdrawal reserved");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to reserve withdrawal"),
                  })
                }
              >
                Reserve
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function ReviewButtons({ id }: { id: string }) {
  const [approveOpen, setApproveOpen] = useState(false);
  const [rejectOpen, setRejectOpen] = useState(false);
  const mutation = useReviewWithdraw();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setApproveOpen(true)}>
        Approve
      </Button>
      <AlertDialog.Backdrop isOpen={approveOpen} onOpenChange={setApproveOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Approve Withdrawal #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>This will approve the withdrawal for processing.</p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button
                variant="tertiary"
                isDisabled={mutation.isPending}
                onPress={() => setApproveOpen(false)}
              >
                Cancel
              </Button>
              <Button
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(
                    { id, approved: true },
                    {
                      onSuccess: () => {
                        toast.success("Withdrawal approved");
                        setApproveOpen(false);
                      },
                      onError: () => toast.danger("Failed to approve withdrawal"),
                    },
                  )
                }
              >
                Approve
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>

      <Button size="sm" variant="ghost" onPress={() => setRejectOpen(true)}>
        Reject
      </Button>
      <AlertDialog.Backdrop isOpen={rejectOpen} onOpenChange={setRejectOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="danger" />
              <AlertDialog.Heading>Reject Withdrawal #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>This will reject the withdrawal.</p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button
                variant="tertiary"
                isDisabled={mutation.isPending}
                onPress={() => setRejectOpen(false)}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() =>
                  mutation.mutate(
                    { id, approved: false },
                    {
                      onSuccess: () => {
                        toast.success("Withdrawal rejected");
                        setRejectOpen(false);
                      },
                      onError: () => toast.danger("Failed to reject withdrawal"),
                    },
                  )
                }
              >
                Reject
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function ConfirmConfirm({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useConfirmWithdraw();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Confirm
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Confirm Withdrawal #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>This will confirm the withdrawal as completed. This action cannot be undone.</p>
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
                  mutation.mutate(id, {
                    onSuccess: () => {
                      toast.success("Withdrawal confirmed");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to confirm withdrawal"),
                  })
                }
              >
                Confirm
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

function RetryConfirm({ id }: { id: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useRetryWithdraw();

  return (
    <>
      <Button size="sm" variant="secondary" onPress={() => setOpen(true)}>
        Retry
      </Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="accent" />
              <AlertDialog.Heading>Retry Withdrawal #{id}?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>This will retry the failed withdrawal by re-reserving funds.</p>
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
                  mutation.mutate(id, {
                    onSuccess: () => {
                      toast.success("Withdrawal retrying");
                      setOpen(false);
                    },
                    onError: () => toast.danger("Failed to retry withdrawal"),
                  })
                }
              >
                Retry
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

export function WithdrawalsPage() {
  const [statusFilter, setStatusFilter] = useState<string>("");
  // Memo the params object so its identity is stable across renders — an inline
  // object would be a new reference every render → cache miss → refetch storm.
  const params = useMemo(() => ({ status: statusFilter || undefined }), [statusFilter]);
  const { data, isLoading, isError, hasNextPage, fetchNextPage, isFetchingNextPage } =
    useWithdrawals(params);
  const withdrawals = data?.pages.flatMap((p) => p.list) ?? [];

  return (
    <div className="space-y-6">
      <PageHeader title="Withdrawals" description="Outbound withdrawal tracking" />

      <div className="flex gap-2">
        <Select
          className="w-40"
          value={statusFilter || "all"}
          onChange={(key) => setStatusFilter(key && key !== "all" ? String(key) : "")}
        >
          <Label className="sr-only">Status</Label>
          <Select.Trigger>
            <Select.Value />
            <Select.Indicator />
          </Select.Trigger>
          <Select.Popover>
            <ListBox>
              <ListBox.Item id="all" textValue="All">
                All
                <ListBox.ItemIndicator />
              </ListBox.Item>
              {WITHDRAW_STATES.map((s) => (
                <ListBox.Item key={s} id={s} textValue={s}>
                  {s}
                  <ListBox.ItemIndicator />
                </ListBox.Item>
              ))}
            </ListBox>
          </Select.Popover>
        </Select>
      </div>

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load withdrawals" />
      ) : withdrawals.length === 0 ? (
        <EmptyState
          title="No withdrawals found"
          description={
            statusFilter ? "Try a different status filter." : "No withdrawals have been created yet."
          }
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Withdrawals" className="min-w-[900px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Holder</Table.Column>
                <Table.Column>Channel</Table.Column>
                <Table.Column className="text-end">Amount</Table.Column>
                <Table.Column>Status</Table.Column>
                <Table.Column>Channel Ref</Table.Column>
                <Table.Column className="text-end">Created</Table.Column>
                <Table.Column>Actions</Table.Column>
              </Table.Header>
              <Table.Body items={withdrawals}>
                {(w) => (
                  <Table.Row id={w.uid}>
                    <Table.Cell className="max-w-32"><span className="block truncate" title={w.uid}>#{w.uid}</span></Table.Cell>
                    <Table.Cell>{w.account_holder}</Table.Cell>
                    <Table.Cell className="max-w-32"><span className="block truncate" title={w.channel_name}>{w.channel_name}</span></Table.Cell>
                    <Table.Cell className="text-end font-mono tabular-nums">
                      {formatAmount(w.amount)}
                    </Table.Cell>
                    <Table.Cell>
                      <StatusChip status={w.status} />
                    </Table.Cell>
                    <Table.Cell className="max-w-40 font-mono text-xs"><span className="block truncate" title={w.channel_ref}>{w.channel_ref || "—"}</span></Table.Cell>
                    <Table.Cell className="text-muted text-end text-xs">
                      {formatUTC(w.created_at)}
                    </Table.Cell>
                    <Table.Cell>
                      <div className="flex flex-wrap gap-1">
                        {w.status === "locked" && <ReserveConfirm id={w.uid} />}
                        {w.status === "reviewing" && <ReviewButtons id={w.uid} />}
                        {w.status === "reserved" && <ProcessModal id={w.uid} />}
                        {w.status === "processing" && (
                          <>
                            <ConfirmConfirm id={w.uid} />
                            <FailModal id={w.uid} />
                          </>
                        )}
                        {w.status === "failed" && <RetryConfirm id={w.uid} />}
                      </div>
                    </Table.Cell>
                  </Table.Row>
                )}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
          {hasNextPage && (
            <Table.Footer className="flex justify-center">
              <Button
                variant="secondary"
                size="sm"
                isPending={isFetchingNextPage}
                onPress={() => fetchNextPage()}
              >
                {isFetchingNextPage ? "Loading..." : "Load More"}
              </Button>
            </Table.Footer>
          )}
        </Table>
      )}
    </div>
  );
}
