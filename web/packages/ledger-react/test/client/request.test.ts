import { http, HttpResponse } from "msw";
import { describe, expect, test } from "vitest";
import { server } from "../setup";
import { ApiRequestError, createLedgerClient } from "../../src/client/client";

const BASE = "http://ledger.test";

describe("request core", () => {
  test("unwraps the envelope and returns data", async () => {
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () =>
        HttpResponse.json({
          code: 0,
          message: "ok",
          data: {
            status: "healthy",
            rollup_queue_depth: 0,
            checkpoint_max_age_seconds: 1,
            active_reservations: 2,
          },
        }),
      ),
    );

    const client = createLedgerClient({ baseUrl: BASE });
    const health = await client.getHealth();
    expect(health.status).toBe("healthy");
    expect(health.active_reservations).toBe(2);
  });

  test("throws ApiRequestError carrying the envelope code on non-2xx", async () => {
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () =>
        HttpResponse.json({ code: 11001, message: "nope" }, { status: 404 }),
      ),
    );

    const client = createLedgerClient({ baseUrl: BASE });
    await expect(client.getHealth()).rejects.toMatchObject({
      name: "ApiRequestError",
      status: 404,
      apiError: { code: 11001, message: "nope" },
    });
    await expect(client.getHealth()).rejects.toBeInstanceOf(ApiRequestError);
  });

  test("falls back to code 19999 when error body is not JSON", async () => {
    server.use(
      http.get(`${BASE}/api/v1/system/health`, () =>
        HttpResponse.text("boom", { status: 500 }),
      ),
    );

    const client = createLedgerClient({ baseUrl: BASE });
    await expect(client.getHealth()).rejects.toMatchObject({
      status: 500,
      apiError: { code: 19999 },
    });
  });
});
