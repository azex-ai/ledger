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

/**
 * Remove `//` and slash-star comments, leaving string and template literals
 * intact.
 *
 * M-14 (W3 adversarial review of the gates): every count in this file was a
 * substring count over raw source, so a COMMENT counted. The reviewer deleted
 * eight of TemplatesPage's nine real `aria-label` attributes and added one
 * comment line repeating the word eight times: the census, the per-skin floor
 * and the shadcn-vs-heroui ratio all still passed, on the page J-12 had
 * specifically hardened.
 *
 * String literals are deliberately NOT stripped: `truncate` and `min-w-0`
 * live inside className strings, which is the thing being counted.
 */
function stripComments(text: string): string {
  let out = "";
  let i = 0;
  let quote: string | null = null;
  while (i < text.length) {
    const ch = text[i];
    const next = text[i + 1];
    if (quote) {
      out += ch;
      if (ch === "\\") {
        out += next ?? "";
        i += 2;
        continue;
      }
      if (ch === quote) quote = null;
      i++;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
      out += ch;
      i++;
      continue;
    }
    if (ch === "/" && next === "/") {
      while (i < text.length && text[i] !== "\n") i++;
      continue;
    }
    if (ch === "/" && next === "*") {
      i += 2;
      while (i < text.length && !(text[i] === "*" && text[i + 1] === "/")) i++;
      i += 2;
      continue;
    }
    out += ch;
    i++;
  }
  return out;
}

function readCode(file: string): string {
  return stripComments(readFileSync(file, "utf8"));
}

function hooksUsedIn(file: string): Set<string> {
  const text = readCode(file);
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
  let text = readCode(path.join(dir, basename));
  for (const sub of subDirs) {
    const subDir = path.join(pkgRoot, "src/components", sub);
    for (const f of tsxFilesIn(subDir)) {
      text += "\n" + readCode(path.join(subDir, f));
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
    // both legitimate).
    //
    // M-14 side effect, worth stating because it changed an answer: counts
    // are now taken over comment-stripped source, and ReconciliationPage's
    // shadcn side turned out to have NO real `onError` at all — its two
    // occurrences are the text of its own `mutation-feedback-allow:` markers.
    // The pair genuinely diverges (shadcn renders isError inline per J-20,
    // heroui toasts from onError); the old counter reported parity because
    // it counted the comments. That divergence is deliberate, so it is
    // classified below rather than silently tolerated.
    const PARITY_EXEMPTIONS: Record<string, Partial<Record<string, string>>> = {
      "ReconciliationPage.tsx": {
        onError:
          "shadcn renders the failure inline from isError (J-20, and its mutation-feedback-allow markers say so); heroui toasts from onError. Both give the user failure feedback; the shapes differ on purpose",
      },
    };

    const violations: string[] = [];
    const usedExemptions = new Set<string>();
    for (const basename of shadcnFiles) {
      const shadcnCounts = countTokensFor(basename, SHADCN_DIR, SHADCN_SUBCOMPONENT_DIRS[basename] ?? []);
      const herouiCounts = countTokensFor(basename, HEROUI_DIR, []);
      for (const token of BEHAVIORAL_TOKENS) {
        if (shadcnCounts[token] === herouiCounts[token]) continue;
        if (PARITY_EXEMPTIONS[basename]?.[token]) {
          usedExemptions.add(`${basename}:${token}`);
          continue;
        }
        violations.push(
          `${basename}: "${token}" count diverges — shadcn=${shadcnCounts[token]} heroui=${herouiCounts[token]}`,
        );
      }
    }
    for (const [basename, tokens] of Object.entries(PARITY_EXEMPTIONS)) {
      for (const token of Object.keys(tokens)) {
        expect(
          usedExemptions.has(`${basename}:${token}`),
          `stale parity exemption ${basename}:${token} — the two skins agree again, so delete the entry`,
        ).toBe(true);
      }
    }
    if (violations.length > 0) {
      throw new Error(`${violations.length} behavioral-token parity violation(s):\n  ${violations.join("\n  ")}`);
    }
  });

  // Hardening ratchet (M3, web audit; tightened J-12, 2026-09-02 web audit).
  // The aria-label / truncate / min-w-0 census per skin can only go UP.
  // These are floors, not targets — RAISE a baseline when a page adds more
  // hardening, NEVER lower one to make a regression pass.
  //
  // J-12 found that the ORIGINAL per-skin-independent floors (21 / 63) were
  // a ratchet that had frozen a 21:63 (1:3) gap in place — commit body
  // language called that "parity" while the gate's own comment admitted
  // "full … token equality is still a longer-term goal". A per-skin-only
  // floor can never catch shadcn falling proportionally further behind a
  // heroui that keeps gaining hardening (raising heroui's floor doesn't
  // raise shadcn's). The SHADCN_MIN_RATIO_OF_HEROUI assertion below closes
  // that gap: it's computed against heroui's CURRENT count, not a frozen
  // baseline, so shadcn's bar rises automatically whenever heroui's does.
  // The J-12 pass itself closed real gaps (13 table aria-labels, TemplatesPage's
  // unlabeled inputs, WithdrawalsPage's un-associated <Label>s, BalancesPage's
  // placeholder-only search field, and TemplatesPage's missing overflow
  // handling), raising shadcn from 21 to 55 against heroui's unchanged 63.
  // M-14, two changes:
  //
  //   - `aria-label` is counted as an ATTRIBUTE (`aria-label=`), over
  //     comment-stripped source. The bare word matched prose, and the
  //     reviewer replaced eight real attributes with one comment naming it
  //     eight times.
  //   - the floor is PER PAGE, not per directory. A directory total lets one
  //     page lose its labels while another gains some -- and TemplatesPage,
  //     the page J-12 hardened, is 14 of shadcn's 47, so it could lose most
  //     of them under a total that another page's growth covers.
  //
  // ⚠️ These numbers are NOT comparable to the pre-M-14 baselines (55/63):
  // the counter changed, not the pages. Measured the same day, the corrected
  // count is 47 shadcn / 63 heroui.
  const HARDENING_TOKENS = ["aria-label=", "truncate", "min-w-0"];
  const SHADCN_PAGE_BASELINE: Record<string, number> = {
    "BalancesPage.tsx": 4,
    "ClassificationsPage.tsx": 2,
    "CurrenciesPage.tsx": 2,
    "DashboardPage.tsx": 0,
    "DepositReviewsPage.tsx": 2,
    "DepositsPage.tsx": 3,
    "JournalDetailPage.tsx": 5,
    "JournalTypesPage.tsx": 2,
    "JournalsPage.tsx": 3,
    "ReconciliationPage.tsx": 1,
    "ReservationsPage.tsx": 2,
    "SnapshotsPage.tsx": 1,
    "SweepMonitorPage.tsx": 3,
    "TemplatesPage.tsx": 14,
    "WithdrawalsPage.tsx": 3,
  };
  const HEROUI_PAGE_BASELINE: Record<string, number> = {
    "BalancesPage.tsx": 4,
    "ClassificationsPage.tsx": 2,
    "CurrenciesPage.tsx": 2,
    "DashboardPage.tsx": 3,
    "DepositReviewsPage.tsx": 3,
    "DepositsPage.tsx": 8,
    "JournalDetailPage.tsx": 1,
    "JournalTypesPage.tsx": 2,
    "JournalsPage.tsx": 3,
    "ReconciliationPage.tsx": 1,
    "ReservationsPage.tsx": 4,
    "SnapshotsPage.tsx": 1,
    "SweepMonitorPage.tsx": 3,
    "TemplatesPage.tsx": 20,
    "WithdrawalsPage.tsx": 6,
  };
  // Re-derived under the corrected counter: 47/63 = 0.746. The old 0.8 was
  // computed from an inflated shadcn count (55), so keeping it would have
  // demanded hardening work to satisfy a measurement error. Raise this as
  // shadcn catches up; never lower it.
  const SHADCN_MIN_RATIO_OF_HEROUI = 0.74;

  function pageCensus(dir: string, file: string): number {
    const text = readCode(path.join(dir, file));
    let n = 0;
    for (const t of HARDENING_TOKENS) {
      n += (text.match(new RegExp(t.replace(/[-/\\^$*+?.()|[\]{}]/g, "\\$&"), "g")) ?? []).length;
    }
    return n;
  }

  function censusFor(dir: string): number {
    return tsxFilesIn(dir).reduce((total, f) => total + pageCensus(dir, f), 0);
  }

  it("hardening census only ratchets up, per page (aria-label= / truncate / min-w-0)", () => {
    for (const [dir, baseline] of [
      [SHADCN_DIR, SHADCN_PAGE_BASELINE],
      [HEROUI_DIR, HEROUI_PAGE_BASELINE],
    ] as const) {
      const files = tsxFilesIn(dir);
      for (const f of files) {
        const floor = baseline[f];
        expect(
          floor,
          `${dir}/${f} has no hardening baseline — add one (its current count) so this page cannot silently lose labels`,
        ).toBeDefined();
        expect(
          pageCensus(dir, f),
          `${dir}/${f}: hardening count fell below its floor. RAISE a baseline when a page gains hardening; never lower one to make a regression pass`,
        ).toBeGreaterThanOrEqual(floor ?? 0);
      }
      for (const f of Object.keys(baseline)) {
        expect(files, `baseline names ${dir}/${f}, which no longer exists — delete the entry`).toContain(f);
      }
    }
  });

  it("shadcn does not fall proportionally behind heroui's CURRENT hardening level (J-12 gate)", () => {
    const shadcnCount = censusFor(SHADCN_DIR);
    const herouiCount = censusFor(HEROUI_DIR);
    const minRequired = herouiCount * SHADCN_MIN_RATIO_OF_HEROUI;
    expect(
      shadcnCount,
      `shadcn=${shadcnCount} heroui=${herouiCount} — shadcn must be >= ${SHADCN_MIN_RATIO_OF_HEROUI * 100}% of heroui's CURRENT count (${minRequired}), not just its own frozen baseline`,
    ).toBeGreaterThanOrEqual(minRequired);
  });
});
