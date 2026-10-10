// The numeric SDK intentionally supports only the exactly representable subset
// of Go's int64 holders. Do not infer identities from rounded JS numbers.
class UnsafeHolderError extends RangeError {
  readonly code = "LEDGER_UNSAFE_HOLDER";

  constructor(readonly location: string) {
    super(`Ledger holder at ${location} must be a safe integer.`);
    this.name = "UnsafeHolderError";
  }
}

function assertSafeHolder(value: unknown, location: string): void {
  if (typeof value !== "number" || !Number.isSafeInteger(value)) {
    throw new UnsafeHolderError(location);
  }
}

type FieldPath = readonly string[];
type HolderFields = readonly FieldPath[];

// Walk only declared DTO fields. In particular, metadata and template amount
// maps can contain arbitrary names/values and are never identity fields.
function validateFields(
  value: unknown,
  fields: HolderFields,
  location: string,
): void {
  function visit(current: unknown, path: FieldPath, at: string): void {
    if (path.length === 0) {
      assertSafeHolder(current, at);
      return;
    }
    const [field, ...rest] = path;
    if (field === "*") {
      if (Array.isArray(current)) {
        current.forEach((item, index) => visit(item, rest, `${at}[${index}]`));
      }
    } else if (
      current !== null && typeof current === "object" &&
      Object.prototype.hasOwnProperty.call(current, field)
    ) {
      visit((current as Record<string, unknown>)[field], rest, `${at}.${field}`);
    }
  }

  fields.forEach((path) => visit(value, path, location));
}

const holderRow: HolderFields = [["account_holder"]];
const holderRows: HolderFields = [["list", "*", "account_holder"]];
const holderEntries: HolderFields = [["entries", "*", "account_holder"]];

const requestFields: Record<string, HolderFields> = {
  "/api/v1/journals": holderEntries,
  "/api/v1/journals/template": [["holder_id"]],
  "/api/v1/balances/batch": [["holder_ids", "*"]],
  "/api/v1/reservations": holderRow,
  "/api/v1/bookings": holderRow,
  "/api/v1/reconcile/account": [["holder"]],
};

// All holder-bearing response shapes used by createLedgerClient. These are
// field selectors, not a general response-schema validator: missing fields
// and other DTO constraints retain their existing handling.
const responseFields: Record<string, HolderFields> = {
  journals: holderEntries,
  entries: holderRows,
  balances: [
    ...holderRow,
    ...holderRows,
    ["classifications", "*", "account_holder"],
    ["list", "*", "holder_id"],
    ["list", "*", "balances", "*", "account_holder"],
  ],
  reservations: [...holderRow, ...holderRows],
  bookings: [...holderRow, ...holderRows],
  deposits: [...holderRow, ...holderRows],
  events: [...holderRow, ...holderRows],
  holders: holderRow,
  templates: holderEntries,
  reconcile: [["details", "*", "account_holder"]],
  snapshots: holderRows,
};

export function validateRequestHolders(
  path: string,
  body: BodyInit | null | undefined,
): void {
  const url = new URL(path, "http://ledger.invalid");
  const segments = url.pathname.split("/");
  const resource = segments[3];
  const holder = segments[4];
  if (
    (resource === "holders" || resource === "balances") &&
    holder !== undefined && holder !== "batch"
  ) {
    assertSafeHolder(Number(holder), "request.path.holder");
  }
  for (const holder of url.searchParams.getAll("holder")) {
    assertSafeHolder(Number(holder), "request.query.holder");
  }

  const fields = requestFields[url.pathname] ?? (
    resource === "templates" && segments[5] === "preview"
      ? [["holder_id"]]
      : []
  );
  if (fields.length > 0 && typeof body === "string") {
    validateFields(JSON.parse(body), fields, "request.body");
  }
}

export function validateResponseHolders(path: string, data: unknown): void {
  const resource = new URL(path, "http://ledger.invalid").pathname.split("/")[3];
  validateFields(data, responseFields[resource] ?? [], "response");
}
