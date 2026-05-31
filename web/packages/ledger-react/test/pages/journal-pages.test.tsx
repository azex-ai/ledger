import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { JournalsPage } from "../../src/components/pages/JournalsPage";
import { JournalDetailPage } from "../../src/components/pages/JournalDetailPage";
import { renderPage, server, getOk } from "./render-page";

function journal(over: Partial<Record<string, unknown>> = {}) {
  return {
    id: 1,
    journal_type_id: 1,
    idempotency_key: "k1",
    total_debit: "100.00",
    total_credit: "100.00",
    metadata: {},
    actor_id: 0,
    source: "api",
    reversal_of: null,
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

describe("JournalsPage", () => {
  test("renders heading and a journal row linking to detail", async () => {
    server.use(
      getOk("/api/v1/journals", { data: [journal({ id: 42 })], next_cursor: "" }),
    );
    renderPage(<JournalsPage />);
    expect(screen.getByRole("heading", { name: "Journals" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#42")).toBeInTheDocument());
    // Default link renderer is a plain anchor to the detail route.
    const link = screen.getByRole("link", { name: "#42" });
    expect(link).toHaveAttribute("href", "/journals/42");
  });
});

describe("JournalDetailPage", () => {
  test("takes id via props and renders the journal heading + entries", async () => {
    server.use(
      getOk("/api/v1/journals/42", {
        journal: journal({ id: 42 }),
        entries: [
          { id: 1, journal_id: 42, account_holder: 1001, currency_id: 1, classification_id: 1, entry_type: "debit", amount: "100.00", created_at: "2026-01-01T00:00:00Z" },
          { id: 2, journal_id: 42, account_holder: -1001, currency_id: 1, classification_id: 2, entry_type: "credit", amount: "100.00", created_at: "2026-01-01T00:00:00Z" },
        ],
      }),
    );
    renderPage(<JournalDetailPage id={42} />);
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Journal #42" })).toBeInTheDocument(),
    );
    expect(screen.getByText("Fund Flow")).toBeInTheDocument();
  });
});
