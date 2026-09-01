import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Static skin-mirror gate (M6, 2026-08-26 web audit). CLAUDE.md states the
 * shadcn skin (src/components/pages) and the HeroUI skin (src/heroui/pages)
 * share the headless core and "page logic must stay mirrored" — but nothing
 * enforced it, and the drift was one-directional (the *default* skin, the
 * package root, was the less-hardened one; see C1). Three cheap, exact
 * static assertions, "readFileSync + regex" in the style of the existing
 * test/styles.test.ts and test/mutation-feedback.test.ts — no parser needed.
 */

const pkgRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

const SHADCN_DIR = path.join(pkgRoot, "src/components/pages");
const HEROUI_DIR = path.join(pkgRoot, "src/heroui/pages");

/**
 * Some pages split their body into sibling components under
 * src/components/<slug>/ on the shadcn side while the HeroUI equivalent
 * stays a single file (DashboardPage → src/components/dashboard/*). Hook
 * parity has to follow the data, not the file split, or a page that
 * delegates to sub-components would look hook-less by comparison. Extend
 * this map if a future page splits the same way.
 */
const SHADCN_SUBCOMPONENT_DIRS: Record<string, string[]> = {
  "DashboardPage.tsx": ["dashboard"],
};

function tsxFilesIn(dir: string): string[] {
  return readdirSync(dir)
    .filter((f) => f.endsWith(".tsx"))
    .sort();
}

function hooksUsedIn(file: string): Set<string> {
  const text = readFileSync(file, "utf8");
  const hooks = new Set<string>();
  // Call sites, not import statements — robust against import aliasing/order.
  for (const m of text.matchAll(/\buse[A-Z]\w*(?=\()/g)) {
    hooks.add(m[0]);
  }
  return hooks;
}

function shadcnHooksFor(basename: string): Set<string> {
  const hooks = hooksUsedIn(path.join(SHADCN_DIR, basename));
  for (const dir of SHADCN_SUBCOMPONENT_DIRS[basename] ?? []) {
    const subDir = path.join(pkgRoot, "src/components", dir);
    for (const f of tsxFilesIn(subDir)) {
      for (const h of hooksUsedIn(path.join(subDir, f))) hooks.add(h);
    }
  }
  return hooks;
}

/**
 * Behavioral tokens counted per page pair. Deliberately narrow: `onError`
 * (explicit toast-on-failure) and a combined `.mutate(`/`.mutateAsync(`
 * count (a page may drive either — ReconciliationPage's HeroUI side uses
 * `toast.promise(mutateAsync(), {...})`, its shadcn side uses
 * `.mutate(...)` + inline `isError`; both are legitimate, so `isError` is
 * NOT in this hard-equality set — it's noisy at the token-count level
 * (most `isError` in these files is unrelated LIST-query loading state, not
 * mutation feedback) and per-call-site correctness is already C1's job
 * (test/mutation-feedback.test.ts), not this gate's).
 */
const BEHAVIORAL_TOKENS = ["onError", "\\.mutate(Async)?\\("] as const;

function countTokensFor(basename: string, dir: string, subDirs: string[]): Record<string, number> {
  let text = readFileSync(path.join(dir, basename), "utf8");
  for (const sub of subDirs) {
    const subDir = path.join(pkgRoot, "src/components", sub);
    for (const f of tsxFilesIn(subDir)) {
      text += "\n" + readFileSync(path.join(subDir, f), "utf8");
    }
  }
  const counts: Record<string, number> = {};
  for (const token of BEHAVIORAL_TOKENS) {
    counts[token] = (text.match(new RegExp(token, "g")) ?? []).length;
  }
  return counts;
}

describe("skin parity (M6 gate)", () => {
  const shadcnFiles = tsxFilesIn(SHADCN_DIR);
  const herouiFiles = tsxFilesIn(HEROUI_DIR);

  it("scanned a non-trivial number of page pairs", () => {
    expect(shadcnFiles.length).toBeGreaterThanOrEqual(14);
  });

  it("file-set parity: every shadcn page has a HeroUI counterpart and vice versa", () => {
    expect(shadcnFiles).toEqual(herouiFiles);
  });

  it("hook-set parity: each page pair calls the same set of hooks", () => {
    const mismatches: string[] = [];
    for (const basename of shadcnFiles) {
      const shadcnHooks = shadcnHooksFor(basename);
      const herouiHooks = hooksUsedIn(path.join(HEROUI_DIR, basename));
      const onlyShadcn = [...shadcnHooks].filter((h) => !herouiHooks.has(h));
      const onlyHeroui = [...herouiHooks].filter((h) => !shadcnHooks.has(h));
      if (onlyShadcn.length > 0 || onlyHeroui.length > 0) {
        mismatches.push(
          `${basename}: shadcn-only=[${onlyShadcn.join(", ")}] heroui-only=[${onlyHeroui.join(", ")}]`,
        );
      }
    }
    if (mismatches.length > 0) {
      throw new Error(
        `${mismatches.length} page pair(s) call different hooks — one skin sees data / has an alarm the other doesn't:\n  ${mismatches.join("\n  ")}`,
      );
    }
  });

  it("behavioral-token parity: onError / isError / .mutate( counts match across each page pair", () => {
    // Absolute correctness (does every onSuccess have a sibling onError?) is
    // C1's job (test/mutation-feedback.test.ts) — this gate is narrower and
    // catches the OTHER half: one skin quietly getting more (or less)
    // failure-feedback machinery than its counterpart, whatever the
    // per-call-site shape (onError toasts vs inline isError rendering are
    // both legitimate; ReconciliationPage's isError-only pattern is
    // symmetric across both skins today and this gate keeps it that way).
    const violations: string[] = [];
    for (const basename of shadcnFiles) {
      const shadcnCounts = countTokensFor(basename, SHADCN_DIR, SHADCN_SUBCOMPONENT_DIRS[basename] ?? []);
      const herouiCounts = countTokensFor(basename, HEROUI_DIR, []);
      for (const token of BEHAVIORAL_TOKENS) {
        if (shadcnCounts[token] !== herouiCounts[token]) {
          violations.push(
            `${basename}: "${token}" count diverges — shadcn=${shadcnCounts[token]} heroui=${herouiCounts[token]}`,
          );
        }
      }
    }
    if (violations.length > 0) {
      throw new Error(`${violations.length} behavioral-token parity violation(s):\n  ${violations.join("\n  ")}`);
    }
  });

  // Hardening ratchet (M3, web audit): the aria-label / truncate / min-w-0
  // census per skin can only go UP. The shadcn (default) skin was the
  // less-hardened one; the M3 pass raised it from 8 to the baseline below.
  // These are floors, not targets — RAISE a baseline when a page adds more
  // hardening, NEVER lower one to make a regression pass. (Full shadcn↔heroui
  // token equality is still a longer-term goal; this gate just stops backslide.)
  const HARDENING_TOKENS = ["aria-label", "truncate", "min-w-0"];
  const CENSUS_BASELINE: Record<string, number> = {
    "src/components/pages": 21,
    "src/heroui/pages": 63,
  };

  function censusFor(dir: string): number {
    let total = 0;
    for (const f of tsxFilesIn(dir)) {
      const text = readFileSync(path.join(dir, f), "utf8");
      for (const t of HARDENING_TOKENS) {
        total += (text.match(new RegExp(t, "g")) ?? []).length;
      }
    }
    return total;
  }

  it("hardening census only ratchets up (aria-label / truncate / min-w-0)", () => {
    expect(censusFor(SHADCN_DIR)).toBeGreaterThanOrEqual(
      CENSUS_BASELINE["src/components/pages"],
    );
    expect(censusFor(HEROUI_DIR)).toBeGreaterThanOrEqual(
      CENSUS_BASELINE["src/heroui/pages"],
    );
  });
});
