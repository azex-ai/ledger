import { screen, waitFor } from "@testing-library/react";
import { describe, expect, test } from "vitest";
import { JournalsPage } from "../../src/components/pages/JournalsPage";
import { JournalDetailPage } from "../../src/components/pages/JournalDetailPage";
import { renderPage, server, getOk } from "./render-page";

function journal(over: Partial<Record<string, unknown>> = {}) {
  return {
    uid: "jr-1",
    journal_type_uid: "jt-1",
    idempotency_key: "k1",
    total_debit: "100.00",
    total_credit: "100.00",
    metadata: {},
    actor_id: 0,
    source: "api",
    reversal_of_uid: "",
    created_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

describe("JournalsPage", () => {
  test("renders heading and a journal row linking to detail", async () => {
    server.use(
      getOk("/api/v1/journals", { list: [journal({ uid: "jr-42" })], next_cursor: "" }),
    );
    renderPage(<JournalsPage />);
    expect(screen.getByRole("heading", { name: "Journals" })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText("#jr-42")).toBeInTheDocument());
    // Default link renderer is a plain anchor to the detail route.
    const link = screen.getByRole("link", { name: "#jr-42" });
    expect(link).toHaveAttribute("href", "/journals/jr-42");
  });
});

describe("JournalDetailPage", () => {
  test("takes id via props and renders the journal heading + entries", async () => {
    server.use(
      getOk("/api/v1/journals/jr-42", {
        journal: journal({ uid: "jr-42" }),
        entries: [
          { journal_uid: "jr-42", account_holder: 1001, currency_uid: "cur-1", classification_uid: "cls-1", entry_type: "debit", amount: "100.00", created_at: "2026-01-01T00:00:00Z" },
          { journal_uid: "jr-42", account_holder: -1001, currency_uid: "cur-1", classification_uid: "cls-2", entry_type: "credit", amount: "100.00", created_at: "2026-01-01T00:00:00Z" },
        ],
      }),
    );
    renderPage(<JournalDetailPage id={"jr-42"} />);
    await waitFor(() =>
      expect(screen.getByRole("heading", { name: "Journal #jr-42" })).toBeInTheDocument(),
    );
    expect(screen.getByText("Fund Flow")).toBeInTheDocument();
  });
});
