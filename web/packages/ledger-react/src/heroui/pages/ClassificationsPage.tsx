"use client";

import { useState } from "react";
import {
  useClassifications,
  useCreateClassification,
  useDeactivateClassification,
} from "../../hooks/use-metadata";
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
import { Tags } from "lucide-react";
import { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "../shared";

function CreateClassificationModal() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState<{ code: string; name: string; normal_side: "debit" | "credit"; is_system: boolean }>({ code: "", name: "", normal_side: "debit", is_system: false });
  const mutation = useCreateClassification();

  function handleSubmit() {
    mutation.mutate(form, {
      onSuccess: () => {
        toast.success("Classification created");
        setOpen(false);
        setForm({ code: "", name: "", normal_side: "debit", is_system: false });
      },
      onError: () => toast.danger("Failed to create classification"),
    });
  }

  return (
    <>
      <Button size="sm" onPress={() => setOpen(true)}>Create</Button>
      <Modal.Backdrop isOpen={open} onOpenChange={setOpen}>
        <Modal.Container>
          <Modal.Dialog className="sm:max-w-md">
            <Modal.CloseTrigger />
            <Modal.Header>
              <Modal.Heading>Create Classification</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <TextField fullWidth name="code" value={form.code} onChange={(v) => setForm({ ...form, code: v })}>
                <Label>Code</Label>
                <Input placeholder="main_wallet" />
              </TextField>
              <TextField fullWidth name="name" value={form.name} onChange={(v) => setForm({ ...form, name: v })}>
                <Label>Name</Label>
                <Input placeholder="Main Wallet" />
              </TextField>
              <Select
                fullWidth
                value={form.normal_side}
                onChange={(v) => { if (typeof v === "string") setForm({ ...form, normal_side: v as "debit" | "credit" }); }}
              >
                <Label>Normal Side</Label>
                <Select.Trigger>
                  <Select.Value />
                  <Select.Indicator />
                </Select.Trigger>
                <Select.Popover>
                  <ListBox>
                    <ListBox.Item id="debit" textValue="Debit">
                      Debit
                      <ListBox.ItemIndicator />
                    </ListBox.Item>
                    <ListBox.Item id="credit" textValue="Credit">
                      Credit
                      <ListBox.ItemIndicator />
                    </ListBox.Item>
                  </ListBox>
                </Select.Popover>
              </Select>
            </Modal.Body>
            <Modal.Footer>
              <Button variant="secondary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                onPress={handleSubmit}
                isPending={mutation.isPending}
                isDisabled={!form.code || !form.name}
              >
                Create
              </Button>
            </Modal.Footer>
          </Modal.Dialog>
        </Modal.Container>
      </Modal.Backdrop>
    </>
  );
}

function DeactivateClassificationDialog({ id, name }: { id: string; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateClassification();

  return (
    <>
      <Button size="sm" variant="tertiary" onPress={() => setOpen(true)}>Deactivate</Button>
      <AlertDialog.Backdrop isOpen={open} onOpenChange={setOpen}>
        <AlertDialog.Container>
          <AlertDialog.Dialog className="sm:max-w-[400px]">
            <AlertDialog.CloseTrigger />
            <AlertDialog.Header>
              <AlertDialog.Icon status="warning" />
              <AlertDialog.Heading>Deactivate &quot;{name}&quot;?</AlertDialog.Heading>
            </AlertDialog.Header>
            <AlertDialog.Body>
              <p>
                This classification will be marked inactive. Existing entries referencing it will be unaffected.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button variant="tertiary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() => mutation.mutate(id, {
                  onSuccess: () => {
                    toast.success("Classification deactivated");
                    setOpen(false);
                  },
                  onError: () => toast.danger("Failed to deactivate classification"),
                })}
              >
                Deactivate
              </Button>
            </AlertDialog.Footer>
          </AlertDialog.Dialog>
        </AlertDialog.Container>
      </AlertDialog.Backdrop>
    </>
  );
}

export function ClassificationsPage() {
  const { data, isLoading, isError } = useClassifications();
  const classifications = Array.isArray(data) ? data : [];

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Classifications" description="Account classification definitions" actions={<CreateClassificationModal />} />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load classifications" />
      ) : classifications.length === 0 ? (
        <EmptyState
          icon={<Tags className="size-8 text-muted" aria-hidden />}
          title="No classifications yet"
          description="Create your first classification to get started."
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Classifications" className="min-w-[720px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Code</Table.Column>
                <Table.Column>Name</Table.Column>
                <Table.Column>Normal Side</Table.Column>
                <Table.Column>System</Table.Column>
                <Table.Column>Active</Table.Column>
                <Table.Column className="text-end">Actions</Table.Column>
              </Table.Header>
              <Table.Body>
                {classifications.map((c) => (
                  <Table.Row key={c.uid} id={c.uid}>
                    <Table.Cell>
                      <span className="block max-w-[160px] truncate font-mono text-xs" title={c.uid}>{c.uid}</span>
                    </Table.Cell>
                    <Table.Cell className="font-mono text-xs">{c.code}</Table.Cell>
                    <Table.Cell>{c.name}</Table.Cell>
                    <Table.Cell><StatusChip status={c.normal_side} /></Table.Cell>
                    <Table.Cell>{c.is_system ? "Yes" : "No"}</Table.Cell>
                    <Table.Cell><StatusChip status={c.is_active ? "active" : "inactive"} /></Table.Cell>
                    <Table.Cell className="text-end">
                      {c.is_active && <DeactivateClassificationDialog id={c.uid} name={c.name} />}
                    </Table.Cell>
                  </Table.Row>
                ))}
              </Table.Body>
            </Table.Content>
          </Table.ScrollContainer>
        </Table>
      )}
    </div>
  );
}
