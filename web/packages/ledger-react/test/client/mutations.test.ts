import { describe, expect, test, vi } from "vitest";
import type { components, paths } from "../../src/client/schema";
import { createLedgerClient, type LedgerClient } from "../../src/client/client";

function captureClient() {
  const fetch = vi.fn<typeof globalThis.fetch>(async () => Response.json({
    code: 200, message: null, data: { entries: [] },
  }));
  const client = createLedgerClient({ baseUrl: "https://ledger.test", fetch });
  function last() {
    const [url, init] = fetch.mock.calls.at(-1)!;
    return {
      path: new URL(String(url)).pathname,
      method: init?.method,
      headers: new Headers(init?.headers),
      body: typeof init?.body === "string" ? JSON.parse(init.body) as unknown : undefined,
    };
  }
  return { client, fetch, last };
}

describe("generated mutation requests use actual wire fields", () => {
  test("raw journal includes event, actor and effective business time", async () => {
    const { client, last } = captureClient();
    const body = {
      journal_type_uid: "type", idempotency_key: "journal-key", event_uid: "event", actor_id: 7,
      source: "operator", effective_at: "2026-10-10T01:02:03Z", metadata: { note: "checked" },
      entries: [{ account_holder: 42, currency_uid: "usd", classification_uid: "available", entry_type: "credit", amount: "123.456" }],
    } satisfies components["schemas"]["JournalInput"];
    await client.postJournal(body);
    expect(last().path).toBe("/api/v1/journals");
    expect(last().body).toEqual(body);
    expect(last().headers.get("Idempotency-Key")).toBe("journal-key");
  });

  test("template journal preserves event, actor, source, metadata and each amount key", async () => {
    const { client, last } = captureClient();
    const body = {
      template_code: "settle", holder_id: 42, currency_uid: "usd", idempotency_key: "template-key",
      event_uid: "event", actor_id: 7, source: "operator", metadata: { note: "settled" },
      amounts: { gross: "100.00", fee: "2.50" },
    } satisfies components["schemas"]["TemplateExecutionRequest"];
    await client.postTemplateJournal(body);
    expect(last().path).toBe("/api/v1/journals/template");
    expect(last().body).toEqual(body);
    expect(last().headers.get("Idempotency-Key")).toBe("template-key");
  });

  test.each([null, { initial: "pending", terminal: ["confirmed"], transitions: { pending: ["confirmed"] } }])(
    "classification preserves lifecycle %j including explicit null", async (lifecycle) => {
      const { client, last } = captureClient();
      const body = {
        code: "available", name: "Available", normal_side: "credit", is_system: false,
        balance_role: "available", lifecycle,
      } satisfies components["schemas"]["ClassificationInput"];
      await client.createClassification(body);
      expect(last().path).toBe("/api/v1/classifications");
      expect(last().body).toEqual(body);
    },
  );

  test("journal type serializes its optional holder_kind", async () => {
    const { client, last } = captureClient();
    const body = { code: "transfer", name: "Transfer", holder_kind: "transfer" } satisfies paths["/journal-types"]["post"]["requestBody"]["content"]["application/json"];
    await client.createJournalType(body);
    expect(last().path).toBe("/api/v1/journal-types");
    expect(last().body).toEqual(body);
  });

  test("template lines can omit the generated optional sort_order", async () => {
    const { client, last } = captureClient();
    const body = {
      code: "settle", name: "Settle", journal_type_uid: "type",
      lines: [{ classification_uid: "available", entry_type: "credit", holder_role: "user", amount_key: "amount" }],
    } satisfies components["schemas"]["TemplateInput"];
    await client.createTemplate(body);
    expect(last().path).toBe("/api/v1/templates");
    expect(last().body).toEqual(body);
  });

  test("currency preserves an explicit zero exponent", async () => {
    const { client, last } = captureClient();
    const body = { code: "POINT", name: "Point", exponent: 0 } satisfies paths["/currencies"]["post"]["requestBody"]["content"]["application/json"];
    await client.createCurrency(body);
    expect(last().path).toBe("/api/v1/currencies");
    expect(last().body).toEqual(body);
  });

  test.each([true, false])("booking includes optional channel/tags/expiry when present=%s", async (present) => {
    const { client, last } = captureClient();
    const body = {
      classification_code: "deposit", account_holder: 42, currency_uid: "usd", amount: "10.00", idempotency_key: "booking-key",
      ...(present ? { channel_name: "evm", metadata: { external_ref: "hash" }, expires_at: "2026-10-11T00:00:00Z" } : {}),
    } satisfies components["schemas"]["CreateBookingInput"];
    await client.createBooking(body);
    expect(last().path).toBe("/api/v1/bookings");
    expect(last().body).toEqual(body);
    expect(last().headers.get("Idempotency-Key")).toBe("booking-key");
  });

  test("booking transition includes source and reconstructs its required wire key from the header", async () => {
    const { client, last } = captureClient();
    const wire = {
      to_status: "confirmed", channel_ref: "tx", amount: "9.50", actor_id: 7, source: "operator",
      metadata: { note: "reviewed" }, idempotency_key: "transition-key",
    } satisfies components["schemas"]["TransitionInput"];
    const { idempotency_key, ...body } = wire;
    await client.transitionBooking("booking", body, idempotency_key);
    expect(last().path).toBe("/api/v1/bookings/booking/transition");
    expect(last().body).toEqual(body);
    expect({ ...last().body as object, idempotency_key: last().headers.get("Idempotency-Key") }).toEqual(wire);
  });
});

describe("preview canonical and convenience inputs", () => {
  test("canonical multi-amount input reaches the server unchanged without mutating or replacing input references", async () => {
    const { client, last } = captureClient();
    const amounts = Object.freeze({ amount: "100.00", fee: "2.50" });
    const body = Object.freeze({ holder_id: 42, currency_uid: "usd", amounts }) satisfies components["schemas"]["TemplatePreviewRequest"];
    await client.previewTemplate("settle", body);
    expect(last().path).toBe("/api/v1/templates/settle/preview");
    expect(last().body).toEqual(body);
    expect(body.amounts).toBe(amounts);
    expect(Object.keys(body)).toEqual(["holder_id", "currency_uid", "amounts"]);
  });

  test("the existing page's single amount is normalized to an amounts map without mutating input", async () => {
    const { client, last } = captureClient();
    const body = Object.freeze({ holder_id: 42, currency_uid: "usd", amount: "100.00" });
    await client.previewTemplate("settle", body);
    expect(last().body).toEqual({ holder_id: 42, currency_uid: "usd", amounts: { amount: "100.00" } });
    expect(body).toEqual({ holder_id: 42, currency_uid: "usd", amount: "100.00" });
    expect("amounts" in body).toBe(false);
  });

  test.each(["1.00", "2.00"])("rejects ambiguous inputs even when amounts agree (%s)", async (amount) => {
    const { client, fetch } = captureClient();
    const input = Object.freeze({ holder_id: 42, currency_uid: "usd", amount, amounts: Object.freeze({ amount: "1.00" }) });
    const promise = client.previewTemplate("settle", input as unknown as Parameters<LedgerClient["previewTemplate"]>[1]);
    expect(promise).toBeInstanceOf(Promise);
    await expect(promise).rejects.toThrow("either amounts or amount, not both");
    expect(fetch).not.toHaveBeenCalled();
    expect(input.amounts).toEqual({ amount: "1.00" });
    expect(input.amount).toBe(amount);
  });

  test("rejects missing amount forms through Promise rejection before fetch", async () => {
    const { client, fetch } = captureClient();
    const input = { holder_id: 42, currency_uid: "usd" } as Parameters<LedgerClient["previewTemplate"]>[1];
    await expect(client.previewTemplate("settle", input)).rejects.toThrow("requires amounts or a single amount");
    expect(fetch).not.toHaveBeenCalled();
  });
});

describe("scalar adapters preserve generated settlement fields and idempotency", () => {
  test("settle carries exact decimal text and required header key", async () => {
    const { client, last } = captureClient();
    const wire = { actual_amount: "9.5000", idempotency_key: "settle-key" } satisfies paths["/reservations/{uid}/settle"]["post"]["requestBody"]["content"]["application/json"];
    await client.settleReservation("reservation", wire.actual_amount, wire.idempotency_key);
    expect(last().path).toBe("/api/v1/reservations/reservation/settle");
    expect({ ...last().body as object, idempotency_key: last().headers.get("Idempotency-Key") }).toEqual(wire);
  });

  test("partial settlement keeps the key in both body and matching header", async () => {
    const { client, last } = captureClient();
    const wire = { amount: "0.0100", idempotency_key: "partial-key" } satisfies paths["/reservations/{uid}/settle-partial"]["post"]["requestBody"]["content"]["application/json"];
    await client.settlePartialReservation("reservation", wire.amount, wire.idempotency_key);
    expect(last().path).toBe("/api/v1/reservations/reservation/settle-partial");
    expect(last().body).toEqual(wire);
    expect(last().headers.get("Idempotency-Key")).toBe(wire.idempotency_key);
  });

  test.each(["finalize", "release"] as const)("%s keeps the required header key without adding a body", async (operation) => {
    const { client, last } = captureClient();
    const wire = { idempotency_key: "terminal-key" } satisfies paths["/reservations/{uid}/finalize"]["post"]["requestBody"]["content"]["application/json"];
    if (operation === "finalize") await client.finalizeReservationSettlement("reservation", wire.idempotency_key);
    else await client.releaseReservation("reservation", wire.idempotency_key);
    expect(last().path).toBe(`/api/v1/reservations/reservation/${operation}`);
    expect(last().body).toBeUndefined();
    expect(last().headers.get("Idempotency-Key")).toBe(wire.idempotency_key);
  });
});
