import { fireEvent, screen, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, test } from "vitest";
import { ReconciliationPage } from "../../src/heroui/pages/ReconciliationPage";
import { renderPage, server, BASE } from "./render-page";

/**
 * M5 guard drift (2026-08-26 web audit): `ReconcileResult.details` is
 * optional per docs/openapi.yaml (no `required:` list on that schema), but
 * the heroui skin dereferenced it unguarded (`accountResult.details.length`)
 * while the shadcn skin correctly guarded it (`accountResult.details &&`).
 * A spec-conformant response omitting `details` crashed only this skin.
 *
 * This drives a real POST /reconcile/account response that omits `details`
 * entirely (spec-conformant per docs/openapi.yaml) and asserts the page
 * renders instead of throwing.
 */
describe("ReconciliationPage (heroui) — guards optional details (M5)", () => {
  test("a balanced result with no details field renders without crashing", async () => {
    server.use(
      http.post(`${BASE}/api/v1/reconcile/account`, () =>
        HttpResponse.json({
          code: 200,
          message: null,
          // Deliberately omits `details` — legal per the optional schema.
          data: { balanced: true, gap: "0", checked_at: "2026-01-01T00:00:00Z" },
        }),
      ),
    );
    renderPage(<ReconciliationPage />);

    fireEvent.change(screen.getByLabelText("Holder"), { target: { value: "1001" } });
    fireEvent.change(screen.getByLabelText("Currency"), { target: { value: "cur-1" } });
    fireEvent.click(screen.getByRole("button", { name: "Check" }));

    await waitFor(() => expect(screen.getByText("Balanced")).toBeInTheDocument());
    // No details table, no thrown error — the page is still on screen.
    expect(screen.getByRole("heading", { name: "Reconciliation" })).toBeInTheDocument();
  });
});
