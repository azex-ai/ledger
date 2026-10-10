import { describe, expect, test, vi } from "vitest";
import { createLedgerClient, type LedgerClient } from "../../src/client/client";

const BASE = "http://ledger.test";
const required = { currency_uid: "currency", amount: "1", idempotency_key: "key" };
type HolderCall = (client: LedgerClient, holder: number) => Promise<unknown>;
const requests: Array<[string, HolderCall]> = [
  ["balance path", (c, h) => c.getBalances(h)],
  ["currency balance path", (c, h) => c.getBalancesByCurrency(h, "currency")],
  ["breakdown path", (c, h) => c.getBalanceBreakdown(h, "currency")],
  ["deposit address path", (c, h) => c.getDepositAddress(h)],
  ["ensure deposit address path", (c, h) => c.ensureDepositAddress(h)],
  ["entries query", (c, h) => c.listEntries({ holder: h })],
  ["reservations query", (c, h) => c.listReservations({ holder: h })],
  ["bookings query", (c, h) => c.listBookings({ holder: h })],
  ["snapshots query", (c, h) => c.listSnapshots({ holder: h })],
  ["batch body", (c, h) => c.batchBalances([1, h], "currency")],
  ["journal entry", (c, h) => c.postJournal({
    journal_type_uid: "journal-type", idempotency_key: "key",
    entries: [{ account_holder: h, currency_uid: "currency",
      classification_uid: "classification", entry_type: "debit", amount: "1" }],
  })],
  ["template journal", (c, h) => c.postTemplateJournal({
    template_code: "template", holder_id: h, currency_uid: "currency",
    idempotency_key: "key", amounts: { amount: "1" },
  })],
  ["reservation body", (c, h) => c.createReservation({ ...required, account_holder: h })],
  ["booking body", (c, h) => c.createBooking({ ...required, account_holder: h,
    classification_code: "deposit", channel_name: "test" })],
  ["preview body", (c, h) => c.previewTemplate("template", {
    holder_id: h, currency_uid: "currency", amount: "1",
  })],
  ["reconcile body", (c, h) => c.reconcileAccount(h, "currency")],
];

function clientWithData(data: unknown) {
  const fetch = vi.fn(async () => Response.json({ code: 200, message: null, data }));
  return { client: createLedgerClient({ baseUrl: BASE, fetch }), fetch };
}

describe("holder request boundary", () => {
  test.each(requests)("%s refuses unsafe holders before fetch", async (_name, call) => {
    const { client, fetch } = clientWithData({ list: [] });
    // The adjacent int64 strings become the SAME JavaScript number. Neither
    // may be sent to an endpoint as a supposedly valid holder.
    for (const value of [
      Number("9007199254740992"), Number("9007199254740993"),
      Number("-9007199254740992"), Number("-9007199254740993"),
      1.5, NaN, Infinity, -Infinity,
    ]) {
      await expect(call(client, value)).rejects.toMatchObject({
        name: "UnsafeHolderError", code: "LEDGER_UNSAFE_HOLDER",
      });
    }
    expect(fetch).not.toHaveBeenCalled();
  });

  test.each(requests)("%s preserves safe bounds and sign/sentinel policy", async (_name, call) => {
    const { client, fetch } = clientWithData({ list: [] });
    // Endpoint-specific positive/negative/zero rules remain the server's job.
    for (const value of [Number.MAX_SAFE_INTEGER, Number.MIN_SAFE_INTEGER, -1, 0, 1]) {
      await call(client, value);
    }
    expect(fetch).toHaveBeenCalledTimes(5);
  });

  test("batch cannot hide an unsafe holder behind a valid first holder", async () => {
    const { client, fetch } = clientWithData({ list: [] });
    await expect(client.batchBalances([1, Number("9007199254740993")], "currency"))
      .rejects.toMatchObject({ location: "request.body.holder_ids[1]" });
    expect(fetch).not.toHaveBeenCalled();
  });

  test("safe system and boundary IDs retain their wire values", async () => {
    const captured: Array<{ url: string; body: unknown }> = [];
    const client = createLedgerClient({ baseUrl: BASE, fetch: async (url, init) => {
      captured.push({ url: String(url), body: init?.body ? JSON.parse(String(init.body)) : undefined });
      return Response.json({ code: 200, message: null, data: { list: [] } });
    } });
    await client.getBalances(Number.MIN_SAFE_INTEGER);
    await client.listEntries({ holder: 0 });
    await client.batchBalances([-1, Number.MAX_SAFE_INTEGER], "currency");
    expect(captured).toEqual([
      { url: `${BASE}/api/v1/balances/-9007199254740991`, body: undefined },
      { url: `${BASE}/api/v1/entries?holder=0`, body: undefined },
      { url: `${BASE}/api/v1/balances/batch`, body: {
        holder_ids: [-1, 9007199254740991], currency_uid: "currency",
      } },
    ]);
  });
});

type ResponseCall = (client: LedgerClient) => Promise<unknown>;
const HOLDER = "__raw_holder__";
const row = { account_holder: HOLDER };
const rows = { list: [{ account_holder: 1 }, row] };
const responses: Array<[string, ResponseCall, unknown]> = [
  ["journal entries", (c) => c.getJournal("journal"), { journal: {}, entries: [row] }],
  ["entries list", (c) => c.listEntries({}), rows],
  ["balance list", (c) => c.getBalances(1), rows],
  ["currency classifications", (c) => c.getBalancesByCurrency(1, "currency"), { classifications: [row] }],
  ["balance breakdown", (c) => c.getBalanceBreakdown(1, "currency"), row],
  ["batch holder", (c) => c.batchBalances([1], "currency"), { list: [{ holder_id: HOLDER, balances: [] }] }],
  ["batch nested balance", (c) => c.batchBalances([1], "currency"), { list: [{ holder_id: 1, balances: [row] }] }],
  ["reservation", (c) => c.createReservation({ ...required, account_holder: 1 }), row],
  ["reservations list", (c) => c.listReservations({}), rows],
  ["booking", (c) => c.getBooking("booking"), row],
  ["create booking", (c) => c.createBooking({ ...required, account_holder: 1,
    classification_code: "deposit", channel_name: "test" }), row],
  ["booking transition", (c) => c.transitionBooking("booking", { to_status: "confirmed" }, "key"), row],
  ["bookings list", (c) => c.listBookings({}), rows],
  ["deposit address", (c) => c.getDepositAddress(1), row],
  ["ensure deposit address", (c) => c.ensureDepositAddress(1), row],
  ["deposit reviews", (c) => c.listDepositReviews({}), rows],
  ["approve deposit", (c) => c.approveDepositReview("booking", "key"), row],
  ["reject deposit", (c) => c.rejectDepositReview("booking", "reason", "key"), row],
  ["event", (c) => c.getEvent("event"), row],
  ["events list", (c) => c.listEvents({}), rows],
  ["template preview", (c) => c.previewTemplate("template", { holder_id: 1, currency_uid: "currency" }), { entries: [row] }],
  ["global reconciliation", (c) => c.reconcileGlobal("key"), { details: [row] }],
  ["account reconciliation", (c) => c.reconcileAccount(1, "currency"), { details: [row] }],
  ["snapshots list", (c) => c.listSnapshots({}), rows],
];

function rawResponseClient(data: unknown, holderLiteral: string) {
  // Do not pass large JS numbers to JSON.stringify here: the wire must retain
  // each original integer before the real Response.json() parses it.
  const body = JSON.stringify({ code: 200, message: null, data })
    .replaceAll(JSON.stringify(HOLDER), holderLiteral);
  return createLedgerClient({ baseUrl: BASE, fetch: async () => new Response(body, {
    headers: { "Content-Type": "application/json" },
  }) });
}

describe("holder response boundary", () => {
  test.each(responses)("%s rejects unsafe IDs after JSON decoding", async (_name, call, data) => {
    for (const literal of ["9007199254740992", "9007199254740993", "-9007199254740993"]) {
      await expect(call(rawResponseClient(data, literal))).rejects.toMatchObject({
        name: "UnsafeHolderError", code: "LEDGER_UNSAFE_HOLDER",
      });
    }
  });

  test.each(responses)("%s preserves exact safe bounds", async (name, call, data) => {
    for (const literal of ["9007199254740991", "-9007199254740991", "0"]) {
      const expected: unknown = JSON.parse(JSON.stringify(data)
        .replaceAll(JSON.stringify(HOLDER), literal));
      const returnsList = ["balance list", "batch holder", "batch nested balance", "snapshots list"].includes(name);
      const result = await call(rawResponseClient(data, literal));
      expect(result).toEqual(returnsList && expected !== null &&
        typeof expected === "object" && "list" in expected ? expected.list : expected);
    }
  });

  test("a response with an unsafe later row rejects the entire page", async () => {
    const client = rawResponseClient(rows, "9007199254740993");
    await expect(client.listEntries({})).rejects.toMatchObject({
      location: "response.list[1].account_holder",
    });
  });

  test.each(['"1"', '"9007199254740993"', "null", "1.5"])(
    "response holder %s cannot masquerade as a numeric integer", async (literal) => {
      await expect(rawResponseClient(row, literal).getBooking("booking"))
        .rejects.toMatchObject({ name: "UnsafeHolderError" });
    },
  );

  test("metadata and actor IDs are not mistaken for holder DTO fields", async () => {
    const metadata = {
      account_holder: Number("9007199254740993"),
      holder_id: "external-id",
      entries: [{ account_holder: "not-a-ledger-holder" }],
    };
    const data = { account_holder: 1, metadata, actor_id: Number("9007199254740993") };
    const { client } = clientWithData(data);
    await expect(client.createBooking({ ...required, account_holder: 1,
      classification_code: "deposit", channel_name: "test", metadata,
    })).resolves.toEqual(data);
  });

  test("template amount keys are not interpreted as identities", async () => {
    const { client } = clientWithData({ entries: [] });
    await expect(client.previewTemplate("template", {
      holder_id: 1, currency_uid: "currency", account_holder: "9007199254740993",
      holder: "large-amount-key",
    })).resolves.toEqual({ entries: [] });
  });
});
