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

// --- C2a (decision B) global-token gates ---------------------------------

// Tailwind v4's standard theme + utility namespaces. These are global by
// design; a custom property under the bare :root/:host that is NOT one of
// these is an unexpected leak worth failing on.
const GLOBAL_TOKEN_ALLOWLIST =
  /^--(?:color|font|font-weight|spacing|container|text|leading|tracking|radius|ease|animate|blur|default)-?/;

// Business design tokens the package themes under `.ledger-root`. None of
// these may ever appear in the global :root/:host scope.
const BUSINESS_TOKEN_RE =
  /^--(?:primary|secondary|background|foreground|accent|muted|border|card|popover|ring|input|destructive|chart|sidebar|success|warning|danger|info)\b/;

// globalScopeTokens returns every custom property declared under a bare
// `:root` or `:host` selector (NOT `.ledger-root`, which is the package's own
// scoped block and legitimately carries business tokens).
function globalScopeTokens(css: string): string[] {
  const tokens: string[] = [];
  const blockRe = /(?:^|[},])\s*(:root|:host)[^{]*\{([^}]*)\}/g;
  let m: RegExpExecArray | null;
  while ((m = blockRe.exec(css)) !== null) {
    for (const p of m[2].matchAll(/(--[a-z0-9-]+)\s*:/g)) tokens.push(p[1]);
  }
  return tokens;
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
  });

  // C2a (decision B, 2026-09-01): Tailwind v4's theme layer (`--color-*`,
  // `--spacing`, `--text-*`, …) and utility layer (`--tw-*` initializers) are
  // global by design and cannot be scoped without abandoning Tailwind's own
  // machinery — option A (scope the theme layer) was rejected as a risky
  // rework of a UI-convenience package with no ledger-correctness stake. What
  // matters, and what these gates enforce, is that the leak is confined to
  // Tailwind's OWN standard namespaces and never carries a BUSINESS token
  // (`--primary`, `--background`, `--chart-*`, …) or a host-affecting reset
  // into the global scope. The business tokens stay `.ledger-root`-scoped
  // (asserted separately below). These assertions fail the moment a business
  // token or an unexpected global custom property appears.
  it("global :root/:host declares only Tailwind standard theme tokens", () => {
    for (const tok of globalScopeTokens(css)) {
      expect(tok).toMatch(GLOBAL_TOKEN_ALLOWLIST);
    }
  });

  it("never leaks a business design token into the global scope", () => {
    for (const tok of globalScopeTokens(css)) {
      expect(tok).not.toMatch(BUSINESS_TOKEN_RE);
    }
  });

  it("the global universal-selector rule initializes only Tailwind --tw-* vars, never resets host elements", () => {
    // Tailwind's utility layer emits a global `*,:before,:after{…}` that seeds
    // its own `--tw-*` custom properties. That is inert for the host (it only
    // defines unused variables). A real reset here (margin/padding/box-sizing
    // on a bare `*`) WOULD reach the host's elements and must never appear.
    const universal = css.match(/\*,:before,:after[^{]*\{([^}]*)\}/);
    if (universal) {
      // Property names starting with a letter are real CSS resets; `--tw-*`
      // custom-property initializers (which start with `-`) are excluded.
      const realResets = [...universal[1].matchAll(/(?:^|;)\s*([a-z][a-z-]*)\s*:/g)].map(
        (m) => m[1],
      );
      expect(realResets).toEqual([]);
    }
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

  // C2a (decision B): the heroui bundle emits the same global Tailwind theme
  // layer. Same gates as the shadcn bundle — Tailwind's own namespaces are
  // allowed global, business tokens are not.
  it("global :root/:host declares only Tailwind standard theme tokens", () => {
    for (const tok of globalScopeTokens(css)) {
      expect(tok).toMatch(GLOBAL_TOKEN_ALLOWLIST);
    }
  });

  it("never leaks a business design token into the global scope", () => {
    for (const tok of globalScopeTokens(css)) {
      expect(tok).not.toMatch(BUSINESS_TOKEN_RE);
    }
  });
});
