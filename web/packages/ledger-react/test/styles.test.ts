import { readFileSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Build-output assertion for the styling pipeline (Phase 5.1).
 *
 * Reads the compiled dist/styles.css and verifies it is:
 *   (a) present and non-empty,
 *   (b) carrying the scoped `.ledger-root` tokens, and
 *   (c) free of Tailwind's global preflight reset (so importing it never
 *       clobbers a host app's global elements).
 *
 * Requires `npm run build` first. The build runs in CI before tests.
 */

const pkgRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);
const stylesPath = path.join(pkgRoot, "dist", "styles.css");

function readStyles(): string {
  try {
    return readFileSync(stylesPath, "utf8");
  } catch {
    throw new Error(
      `dist/styles.css not found at ${stylesPath}. Run \`npm run build\` before tests.`,
    );
  }
}

describe("dist/styles.css", () => {
  const css = readStyles();

  it("exists and is non-empty", () => {
    expect(css.length).toBeGreaterThan(0);
  });

  it("contains the scoped .ledger-root tokens", () => {
    expect(css).toContain(".ledger-root");
    // A representative token must be present and scoped.
    expect(css).toMatch(/\.ledger-root[^{]*\{[^}]*--primary:/);
  });

  it("emits utilities the components use", () => {
    // Scanned from src/**/*.{ts,tsx}; proves the @source scan ran.
    expect(css).toContain(".bg-background");
    expect(css).toContain(".text-foreground");
    expect(css).toContain(".border-border");
  });

  it("uses a class-based dark: variant, not OS media query", () => {
    // dark: utilities must key off the .ledger-root wrapper class.
    expect(css).toContain(".ledger-root:not(.light)");
    // ...and must NOT be gated behind the OS color-scheme media query.
    expect(css).not.toContain("prefers-color-scheme");
  });

  it("emits the font-heading utility", () => {
    // font-heading is used by alert-dialog/card/sheet/dialog; the --font-*
    // theme mapping must make it generate.
    expect(css).toContain(".font-heading{");
  });

  it("does NOT contain Tailwind's global preflight reset", () => {
    // Preflight's hallmark global box-sizing reset.
    expect(css).not.toContain("box-sizing:border-box");
    // Preflight resets bare html/body globally.
    expect(css).not.toMatch(/(^|[^a-zA-Z._#-])html\s*\{/);
    expect(css).not.toContain("body{margin:0");
    // Preflight zeroes heading/blockquote/etc. margins globally.
    expect(css).not.toMatch(/(^|[},])(h1|h2|h3|h4|h5|h6|p|blockquote)[^{]*\{margin:0/);
  });
});
