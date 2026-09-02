import { readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { LEDGER_NAV_ITEMS } from "../src/components/nav";

/*
 * C1 gate: the package ships LEDGER_NAV_ITEMS (the sidebar contract), and the
 * dogfood Next app in web/src/app must have a route for every non-separator
 * href. Two hrefs (/deposit-reviews, /sweeps) once pointed at pages that were
 * never created here, so the sidebar 404'd on the human-review queue.
 *
 * This is the one seam none of the in-package gates covered: package nav
 * contract vs. host route table. It runs in the package's own CI test step,
 * globbing the sibling app tree.
 */

const appRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
  "..",
  "..",
  "src",
  "app",
);

/** Walk src/app, return the route path of every page.tsx (route groups stripped). */
function collectAppRoutes(dir: string, segments: string[] = []): string[] {
  const routes: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    if (entry.isDirectory()) {
      // Route groups `(name)` don't contribute a URL segment.
      const seg = /^\(.*\)$/.test(entry.name) ? null : entry.name;
      routes.push(
        ...collectAppRoutes(
          path.join(dir, entry.name),
          seg === null ? segments : [...segments, seg],
        ),
      );
    } else if (entry.name === "page.tsx") {
      routes.push("/" + segments.join("/") || "/");
    }
  }
  return routes;
}

// Routes that legitimately have no sidebar entry (J-25, 2026-09-02 web
// audit): the original C1 gate only checked nav→route, so a route added to
// the app with no nav entry — dead code the user can only reach by typing
// the URL — would pass silently. Each entry here needs a reason, same
// discipline as the other `*-allow` markers in this suite.
const EXPECTED_UNLISTED = new Set([
  "/", // dashboard — nav's "/" entry IS this route; kept out of navHrefs by
  // the filter above only in the sense that "/" appears both as a nav href
  // and an app route, so it's already covered — listed here defensively in
  // case that ever changes.
  "/login", // auth entry point, reached by redirect, not sidebar navigation
  "/journals/[id]", // detail route, reached by clicking a row in /journals, not the sidebar
]);

describe("sidebar nav contract vs. dogfood app routes", () => {
  const appRoutes = new Set(collectAppRoutes(appRoot));
  const navHrefs = LEDGER_NAV_ITEMS.filter(
    (i): i is Extract<typeof i, { href: string }> => i.type === undefined,
  ).map((i) => i.href);

  it("every nav href has a page.tsx in web/src/app", () => {
    const missing = navHrefs.filter((h) => !appRoutes.has(h));
    expect(missing).toEqual([]);
  });

  // J-25: the reverse direction — a route with no nav entry is dead code a
  // user can only reach by typing the URL; the original gate was blind to it.
  it("every app route either has a nav entry or is explicitly allowlisted with a reason", () => {
    const navHrefSet = new Set(navHrefs);
    const unlisted = [...appRoutes].filter(
      (r) => !navHrefSet.has(r) && !EXPECTED_UNLISTED.has(r),
    );
    expect(unlisted).toEqual([]);
  });
});
