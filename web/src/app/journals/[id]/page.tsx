import { JournalDetailPage } from "@azex/ledger-react";
import { NextLink } from "@/components/next-link";

export default async function Page({
  params,
}: {
  params: Promise<{ id: string }>;
}) {
  const { id } = await params;
  return <JournalDetailPage id={Number(id)} linkComponent={NextLink} />;
}
