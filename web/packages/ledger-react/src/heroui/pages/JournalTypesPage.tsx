"use client";

import { useState } from "react";
import {
  useJournalTypes,
  useCreateJournalType,
  useDeactivateJournalType,
} from "../../hooks/use-metadata";
import {
  AlertDialog,
  Button,
  Input,
  Label,
  Modal,
  Table,
  TextField,
  toast,
} from "@heroui/react";
import { FileType2 } from "lucide-react";
import { PageHeader, EmptyState, ErrorState, StatusChip, TableSkeleton } from "../shared";

function CreateJournalTypeModal() {
  const [open, setOpen] = useState(false);
  const [form, setForm] = useState({ code: "", name: "" });
  const mutation = useCreateJournalType();

  function handleSubmit() {
    mutation.mutate(form, {
      onSuccess: () => {
        toast.success("Journal type created");
        setOpen(false);
        setForm({ code: "", name: "" });
      },
      onError: () => toast.danger("Failed to create journal type"),
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
              <Modal.Heading>Create Journal Type</Modal.Heading>
            </Modal.Header>
            <Modal.Body className="flex flex-col gap-4">
              <TextField fullWidth name="code" value={form.code} onChange={(v) => setForm({ ...form, code: v })}>
                <Label>Code</Label>
                <Input placeholder="deposit" />
              </TextField>
              <TextField fullWidth name="name" value={form.name} onChange={(v) => setForm({ ...form, name: v })}>
                <Label>Name</Label>
                <Input placeholder="Deposit Confirmation" />
              </TextField>
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

function DeactivateJournalTypeDialog({ id, name }: { id: string; name: string }) {
  const [open, setOpen] = useState(false);
  const mutation = useDeactivateJournalType();

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
                This journal type will be marked inactive. Existing journals using it will be unaffected.
              </p>
            </AlertDialog.Body>
            <AlertDialog.Footer>
              <Button variant="tertiary" isDisabled={mutation.isPending} onPress={() => setOpen(false)}>Cancel</Button>
              <Button
                variant="danger"
                isPending={mutation.isPending}
                onPress={() => mutation.mutate(id, {
                  onSuccess: () => {
                    toast.success("Journal type deactivated");
                    setOpen(false);
                  },
                  onError: () => toast.danger("Failed to deactivate journal type"),
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

export function JournalTypesPage() {
  const { data, isLoading, isError } = useJournalTypes();
  const types = Array.isArray(data) ? data : [];

  return (
    <div className="flex flex-col gap-6">
      <PageHeader title="Journal Types" description="Journal type definitions" actions={<CreateJournalTypeModal />} />

      {isLoading ? (
        <TableSkeleton rows={5} />
      ) : isError ? (
        <ErrorState message="Failed to load journal types" />
      ) : types.length === 0 ? (
        <EmptyState
          icon={<FileType2 className="size-8 text-muted" aria-hidden />}
          title="No journal types yet"
          description="Create your first journal type to get started."
        />
      ) : (
        <Table>
          <Table.ScrollContainer>
            <Table.Content aria-label="Journal types" className="min-w-[640px]">
              <Table.Header>
                <Table.Column isRowHeader>ID</Table.Column>
                <Table.Column>Code</Table.Column>
                <Table.Column>Name</Table.Column>
                <Table.Column>Active</Table.Column>
                <Table.Column>Created</Table.Column>
                <Table.Column className="text-end">Actions</Table.Column>
              </Table.Header>
              <Table.Body>
                {types.map((t) => (
                  <Table.Row key={t.uid} id={t.uid}>
                    <Table.Cell>
                      <span className="block max-w-[160px] truncate font-mono text-xs" title={t.uid}>{t.uid}</span>
                    </Table.Cell>
                    <Table.Cell className="font-mono text-xs">{t.code}</Table.Cell>
                    <Table.Cell>{t.name}</Table.Cell>
                    <Table.Cell><StatusChip status={t.is_active ? "active" : "inactive"} /></Table.Cell>
                    <Table.Cell className="text-xs text-muted">{new Date(t.created_at).toLocaleDateString()}</Table.Cell>
                    <Table.Cell className="text-end">
                      {t.is_active && <DeactivateJournalTypeDialog id={t.uid} name={t.name} />}
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
