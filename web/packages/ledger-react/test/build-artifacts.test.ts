import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Build-output assertion (J-9, 2026-09-02 web audit): `formatAmount` and
 * friends were a previously zero-caller export from any package entry —
 * `grep -o "formatAmount|parseUnits|shortenAddress" dist/*.d.ts` came back
 * empty even though every hook returns raw NUMERIC(30,18) strings a headless
 * consumer has no other sane way to display. This package's CI
 * (.github/workflows/ledger-react.yml, outside this worker's file scope —
 * D-tests owns it) runs a separate shell "Assert build artifacts" step
 * AFTER this vitest suite; this test gives the same invariant a home inside
 * the suite itself, in the style of the existing dist-reading
 * test/styles.test.ts, so a regression here fails at the earliest possible
 * point in CI rather than only in that later shell step.
 *
 * Requires `npm run build` first, same precondition as test/styles.test.ts.
 */

const pkgRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

function readDts(name: string): string {
  const p = path.join(pkgRoot, "dist", name);
  try {
    return readFileSync(p, "utf8");
  } catch {
    throw new Error(`dist/${name} not found at ${p}. Run \`npm run build\` before tests.`);
  }
}

describe("dist/*.d.ts export formatAmount from every entry that ships display helpers", () => {
  it.each([
    "index.d.ts",
    "headless.d.ts",
    "heroui.d.ts",
    "wallet.d.ts",
    "wallet-headless.d.ts",
    "wallet-heroui.d.ts",
  ])("%s", (file) => {
    const dts = readDts(file);
    expect(dts).toContain("formatAmount");
    expect(dts).toContain("parseUnits");
    expect(dts).toContain("shortenAddress");
  });
});
