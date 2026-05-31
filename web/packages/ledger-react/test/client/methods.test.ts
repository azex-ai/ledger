import { http, HttpResponse } from "msw";
import { describe, expect, test } from "vitest";
import { server } from "../setup";
import { createLedgerClient } from "../../src/client/client";

const BASE = "http://ledger.test";
const API_KEY = "secret-key";

// Captures the request the client made, then replies with a wrapped envelope.
// `respond` is the unwrapped `data` payload (or null for 204).
interface Captured {
  url: string;
  method: string;
  auth: string | null;
  body: unknown;
}

function intercept(
  method: "get" | "post",
  path: string,
  respond: unknown,
  status = 200,
): { captured: () => Captured | undefined } {
  let captured: Captured | undefined;
  server.use(
    http[method](`${BASE}${path}`, async ({ request }) => {
      let body: unknown = undefined;
      const text = await request.text();
      if (text) body = JSON.parse(text);
      captured = {
        url: request.url,
        method: request.method,
        auth: request.headers.get("authorization"),
        body,
      };
      if (status === 204) return new HttpResponse(null, { status: 204 });
      return HttpResponse.json({ code: 0, message: "ok", data: respond });
    }),
  );
  return { captured: () => captured };
}

const client = createLedgerClient({ baseUrl: BASE, apiKey: API_KEY });
// A no-key client to assert Authorization is omitted when apiKey is unset.
const noKey = createLedgerClient({ baseUrl: BASE });

describe("system", () => {
  test("getSystemBalances", async () => {
    const i = intercept("get", "/api/v1/system/balances", []);
    await client.getSystemBalances();
    expect(i.captured()?.method).toBe("GET");
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/system/balances`);
  });
});

describe("journals + entries", () => {
  test("listJournals encodes querystring", async () => {
    const i = intercept("get", "/api/v1/journals", { data: [], next_cursor: "" });
    await client.listJournals({ cursor: "abc", limit: 10 });
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/journals?cursor=abc&limit=10`);
  });

  test("getJournal", async () => {
    const i = intercept("get", "/api/v1/journals/7", {});
    await client.getJournal(7);
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/journals/7`);
  });

  test("postJournal attaches Bearer and serializes body", async () => {
    const i = intercept("post", "/api/v1/journals", {});
    await client.postJournal({
      journal_type_id: 1,
      idempotency_key: "k1",
      entries: [],
    });
    const c = i.captured();
    expect(c?.method).toBe("POST");
    expect(c?.auth).toBe(`Bearer ${API_KEY}`);
    expect(c?.body).toMatchObject({ journal_type_id: 1, idempotency_key: "k1" });
  });

  test("postJournal omits Bearer when no apiKey", async () => {
    const i = intercept("post", "/api/v1/journals", {});
    await noKey.postJournal({
      journal_type_id: 1,
      idempotency_key: "k1",
      entries: [],
    });
    expect(i.captured()?.auth).toBeNull();
  });

  test("postTemplateJournal", async () => {
    const i = intercept("post", "/api/v1/journals/template", {});
    await client.postTemplateJournal({
      template_code: "dep",
      holder_id: 1,
      currency_id: 1,
      idempotency_key: "k",
      amounts: { gross: "10" },
    });
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/journals/template`);
    expect(i.captured()?.auth).toBe(`Bearer ${API_KEY}`);
  });

  test("reverseJournal", async () => {
    const i = intercept("post", "/api/v1/journals/5/reverse", {});
    await client.reverseJournal(5, "oops");
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/journals/5/reverse`);
    expect(i.captured()?.body).toEqual({ reason: "oops" });
  });

  test("listEntries", async () => {
    const i = intercept("get", "/api/v1/entries", { data: [], next_cursor: "" });
    await client.listEntries({ holder: 3, currency_id: 1 });
    expect(i.captured()?.url).toBe(
      `${BASE}/api/v1/entries?holder=3&currency_id=1`,
    );
  });
});

describe("balances", () => {
  test("getBalances + getBalancesByCurrency", async () => {
    const a = intercept("get", "/api/v1/balances/9", []);
    await client.getBalances(9);
    expect(a.captured()?.url).toBe(`${BASE}/api/v1/balances/9`);

    const b = intercept("get", "/api/v1/balances/9/2", []);
    await client.getBalancesByCurrency(9, 2);
    expect(b.captured()?.url).toBe(`${BASE}/api/v1/balances/9/2`);
  });

  test("batchBalances POST body", async () => {
    const i = intercept("post", "/api/v1/balances/batch", {});
    await client.batchBalances([1, 2], 3);
    expect(i.captured()?.method).toBe("POST");
    expect(i.captured()?.auth).toBe(`Bearer ${API_KEY}`);
    expect(i.captured()?.body).toEqual({ holder_ids: [1, 2], currency_id: 3 });
  });
});

describe("reservations", () => {
  test("listReservations", async () => {
    const i = intercept("get", "/api/v1/reservations", []);
    await client.listReservations({ holder: 1, status: "active" });
    expect(i.captured()?.url).toBe(
      `${BASE}/api/v1/reservations?holder=1&status=active`,
    );
  });

  test("createReservation", async () => {
    const i = intercept("post", "/api/v1/reservations", {});
    await client.createReservation({
      account_holder: 1,
      currency_id: 1,
      amount: "5",
      idempotency_key: "k",
    });
    expect(i.captured()?.auth).toBe(`Bearer ${API_KEY}`);
    expect(i.captured()?.body).toMatchObject({ amount: "5" });
  });

  test("settleReservation + releaseReservation (204)", async () => {
    const s = intercept("post", "/api/v1/reservations/4/settle", null, 204);
    await client.settleReservation(4, "3");
    expect(s.captured()?.body).toEqual({ actual_amount: "3" });

    const r = intercept("post", "/api/v1/reservations/4/release", null, 204);
    await client.releaseReservation(4);
    expect(r.captured()?.method).toBe("POST");
  });
});

describe("bookings", () => {
  test("createBooking", async () => {
    const i = intercept("post", "/api/v1/bookings", {});
    await client.createBooking({
      classification_code: "deposit",
      account_holder: 1,
      currency_id: 1,
      amount: "10",
      idempotency_key: "k",
      channel_name: "evm",
    });
    expect(i.captured()?.auth).toBe(`Bearer ${API_KEY}`);
    expect(i.captured()?.body).toMatchObject({ classification_code: "deposit" });
  });

  test("transitionBooking", async () => {
    const i = intercept("post", "/api/v1/bookings/8/transition", {});
    await client.transitionBooking(8, { to_status: "confirmed" });
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/bookings/8/transition`);
    expect(i.captured()?.body).toEqual({ to_status: "confirmed" });
  });

  test("getBooking + listBookings", async () => {
    const g = intercept("get", "/api/v1/bookings/8", {});
    await client.getBooking(8);
    expect(g.captured()?.url).toBe(`${BASE}/api/v1/bookings/8`);

    const l = intercept("get", "/api/v1/bookings", { data: [], next_cursor: "" });
    await client.listBookings({ holder: 1, status: "pending" });
    expect(l.captured()?.url).toBe(
      `${BASE}/api/v1/bookings?holder=1&status=pending`,
    );
  });
});

describe("events", () => {
  test("getEvent + listEvents", async () => {
    const g = intercept("get", "/api/v1/events/2", {});
    await client.getEvent(2);
    expect(g.captured()?.url).toBe(`${BASE}/api/v1/events/2`);

    const l = intercept("get", "/api/v1/events", { data: [], next_cursor: "" });
    await client.listEvents({ classification_code: "deposit", limit: 5 });
    expect(l.captured()?.url).toBe(
      `${BASE}/api/v1/events?classification_code=deposit&limit=5`,
    );
  });
});

describe("classifications", () => {
  test("list / create / deactivate", async () => {
    const l = intercept("get", "/api/v1/classifications", []);
    await client.listClassifications(true);
    expect(l.captured()?.url).toBe(
      `${BASE}/api/v1/classifications?active_only=true`,
    );

    const c = intercept("post", "/api/v1/classifications", {});
    await client.createClassification({
      code: "x",
      name: "X",
      normal_side: "debit",
      is_system: false,
    });
    expect(c.captured()?.auth).toBe(`Bearer ${API_KEY}`);

    const d = intercept(
      "post",
      "/api/v1/classifications/3/deactivate",
      null,
      204,
    );
    await client.deactivateClassification(3);
    expect(d.captured()?.method).toBe("POST");
  });
});

describe("journal-types", () => {
  test("list / create / deactivate", async () => {
    const l = intercept("get", "/api/v1/journal-types", []);
    await client.listJournalTypes(true);
    expect(l.captured()?.url).toBe(
      `${BASE}/api/v1/journal-types?active_only=true`,
    );

    const c = intercept("post", "/api/v1/journal-types", {});
    await client.createJournalType({ code: "jt", name: "JT" });
    expect(c.captured()?.body).toEqual({ code: "jt", name: "JT" });

    const d = intercept("post", "/api/v1/journal-types/2/deactivate", null, 204);
    await client.deactivateJournalType(2);
    expect(d.captured()?.method).toBe("POST");
  });
});

describe("templates", () => {
  test("list / create / deactivate / preview", async () => {
    const l = intercept("get", "/api/v1/templates", []);
    await client.listTemplates();
    expect(l.captured()?.url).toBe(`${BASE}/api/v1/templates`);

    const c = intercept("post", "/api/v1/templates", {});
    await client.createTemplate({
      code: "t",
      name: "T",
      journal_type_id: 1,
      lines: [],
    });
    expect(c.captured()?.auth).toBe(`Bearer ${API_KEY}`);

    const d = intercept("post", "/api/v1/templates/9/deactivate", null, 204);
    await client.deactivateTemplate(9);
    expect(d.captured()?.method).toBe("POST");

    const p = intercept("post", "/api/v1/templates/dep/preview", {
      entries: [],
      total_debit: "0",
      total_credit: "0",
    });
    await client.previewTemplate("dep", { holder_id: 1, currency_id: 1 });
    expect(p.captured()?.url).toBe(`${BASE}/api/v1/templates/dep/preview`);
    expect(p.captured()?.body).toMatchObject({ holder_id: 1 });
  });
});

describe("currencies", () => {
  test("listCurrencies with activeOnly", async () => {
    const i = intercept("get", "/api/v1/currencies", []);
    await client.listCurrencies(true);
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/currencies?active_only=true`);
  });

  test("listCurrencies without args sends no querystring", async () => {
    const i = intercept("get", "/api/v1/currencies", []);
    await client.listCurrencies();
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/currencies`);
  });

  test("createCurrency", async () => {
    const i = intercept("post", "/api/v1/currencies", {});
    await client.createCurrency({ code: "USD", name: "Dollar" });
    expect(i.captured()?.auth).toBe(`Bearer ${API_KEY}`);
    expect(i.captured()?.body).toEqual({ code: "USD", name: "Dollar" });
  });

  test("deactivateCurrency", async () => {
    const i = intercept("post", "/api/v1/currencies/4/deactivate", null, 204);
    await client.deactivateCurrency(4);
    expect(i.captured()?.url).toBe(`${BASE}/api/v1/currencies/4/deactivate`);
    expect(i.captured()?.method).toBe("POST");
  });
});

describe("reconciliation", () => {
  test("reconcileGlobal + reconcileAccount", async () => {
    const g = intercept("post", "/api/v1/reconcile", {});
    await client.reconcileGlobal();
    expect(g.captured()?.method).toBe("POST");
    expect(g.captured()?.auth).toBe(`Bearer ${API_KEY}`);

    const a = intercept("post", "/api/v1/reconcile/account", {});
    await client.reconcileAccount(1, 2);
    expect(a.captured()?.body).toEqual({ holder: 1, currency_id: 2 });
  });
});

describe("snapshots", () => {
  test("listSnapshots", async () => {
    const i = intercept("get", "/api/v1/snapshots", []);
    await client.listSnapshots({ holder: 1, start: "2026-01-01" });
    expect(i.captured()?.url).toBe(
      `${BASE}/api/v1/snapshots?holder=1&start=2026-01-01`,
    );
  });
});
