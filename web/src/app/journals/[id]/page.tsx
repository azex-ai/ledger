import { JournalDetailClient } from "./_components/journal-detail-client";

export default function JournalDetailPage({ params }: { params: Promise<{ id: string }> }) {
  return <JournalDetailClient params={params} />;
}
