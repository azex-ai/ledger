import { Suspense } from "react";
import { JournalDetailClient } from "./_components/journal-detail-client";

export default function JournalDetailPage({ params }: { params: Promise<{ id: string }> }) {
  return (
    <Suspense fallback={<div className="space-y-4"><div className="h-8 w-48 animate-shimmer rounded" /><div className="h-64 animate-shimmer rounded" /></div>}>
      <JournalDetailClient params={params} />
    </Suspense>
  );
}
