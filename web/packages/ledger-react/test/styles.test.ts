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

  it("keys dark: off appearance classes: explicit .dark + system media", () => {
    // Explicit dark class...
    expect(css).toContain(".ledger-root.dark");
    // ...and system mode (default): .system gated behind the OS media query.
    expect(css).toContain(".ledger-root.system");
    expect(css).toContain("prefers-color-scheme");
  });

  it("dark tokens are single-source via light-dark(), appearance classes only flip color-scheme", () => {
    // theme.css writes every color token ONCE as light-dark(light, dark); the
    // .dark / .system selectors must contain nothing but the color-scheme
    // flip — a token redeclared there would silently shadow the single source.
    const grab = (re: RegExp) => {
      const m = css.match(re);
      if (!m) throw new Error(`token block not found: ${re}`);
      return m[1];
    };
    expect(css.match(/light-dark\(/g)?.length ?? 0).toBeGreaterThan(10);
    const dark = grab(/\.ledger-root\.dark\{([^}]*)\}/);
    const system = grab(/\.ledger-root\.system\{([^}]*)\}/);
    for (const block of [dark, system]) {
      expect(block).toContain("color-scheme:dark");
      expect(block).not.toContain("--");
    }
  });

  it("emits the font-heading utility", () => {
    // font-heading is used by alert-dialog/card/sheet/dialog; the --font-*
    // theme mapping must make it generate.
    expect(css).toContain(".font-heading{");
  });

  it("ships a scoped preflight, never a global one", () => {
    // The reset must exist (self-contained rendering in bare hosts)...
    expect(css).toContain("box-sizing:border-box");
    expect(css).toContain("border-collapse:collapse");
    // ...but only under .ledger-root — bare html/body/h1 selectors that would
    // reset the HOST app's elements must never appear.
    expect(css).not.toMatch(/(^|[^a-zA-Z._#-])html\s*\{/);
    expect(css).not.toContain("body{margin:0");
    expect(css).not.toMatch(/(^|[},])(h1|h2|h3|h4|h5|h6|p|blockquote)[^{]*\{margin:0/);
    expect(css).not.toMatch(/(^|[},])\*\s*[,{]/); // bare universal selector
  });

  it("paints its own base: font, background, foreground on .ledger-root", () => {
    expect(css).toMatch(/\.ledger-root[^{]*\{[^}]*font-family:var\(--font-sans\)/);
    expect(css).toMatch(/\.ledger-root[^{]*\{[^}]*background-color:var\(--background\)/);
    expect(css).toMatch(/\.ledger-root[^{]*\{[^}]*color:var\(--foreground\)/);
  });
});

describe("dist/heroui.css", () => {
  const herouiPath = path.join(pkgRoot, "dist", "heroui.css");
  const css = (() => {
    try {
      return readFileSync(herouiPath, "utf8");
    } catch {
      throw new Error(
        `dist/heroui.css not found at ${herouiPath}. Run \`npm run build\` before tests.`,
      );
    }
  })();

  it("exists and emits layout utilities the heroui skin uses", () => {
    expect(css.length).toBeGreaterThan(0);
    // Structural classes scanned from src/heroui/** — representative sample.
    expect(css).toContain(".flex");
    expect(css).toContain(".min-w-0");
    expect(css).toContain(".truncate");
  });

  it("ships no preflight and no ledger tokens (host owns the HeroUI theme)", () => {
    // No global element resets…
    expect(css).not.toMatch(/(^|[^a-zA-Z._#-])html\s*\{/);
    expect(css).not.toContain("body{margin:0");
    // …and no .ledger-root token block — theming belongs to @heroui/styles.
    expect(css).not.toMatch(/\.ledger-root[^{]*\{[^}]*--primary:/);
  });
});
