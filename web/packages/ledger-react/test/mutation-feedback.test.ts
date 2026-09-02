import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Static gate for C1 (2026-08-26 web audit) — "money-path mutation failures
 * are completely silent." A `.mutate(...)`/`.mutateAsync(...)` call that
 * hands `useMutation` an `onSuccess` but no `onError` is a silent-failure bug
 * by construction: the button stops spinning, the dialog closes on click
 * (AlertDialogAction), and nothing tells the operator the write failed
 * (nextjs.md: "沉默 = bug").
 *
 * Rule enforced: for every `.mutate(`/`.mutateAsync(` call site in ANY tsx
 * under src/ (J-20, 2026-09-02 web audit — widened from just the two pages
 * dirs, which missed src/wallet/** and src/components/dashboard/**), if its
 * (paren-balanced) argument list contains `onSuccess`, it must also contain
 * `onError` in that SAME argument list.
 *
 * Calls with NEITHER onSuccess nor onError (e.g. ReconciliationPage's bare
 * `mutation.mutate()`, fed back via inline `.isError` rendering instead of a
 * toast) are a legitimate, different feedback pattern — but J-20 tightens the
 * exemption from an unconditional pass to requiring a same-call-site
 * `mutation-feedback-allow:` comment directly above or on the call's line,
 * so a NEW page dropped in `dashboard/`/`wallet/` (previously unscanned, and
 * the exact blind spot this gate's widening targets) can't silently reuse the
 * "neither" shape to skip feedback entirely — it must say why.
 *
 * "readFileSync + regex" in the style of the existing test/styles.test.ts —
 * no parser needed for a heuristic this exact (audit's own suggested design).
 */

const pkgRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

/** Extract the balanced-parens snippet starting at text[openIndex] === "(". */
function extractBalanced(text: string, openIndex: number): string {
  let depth = 0;
  let inString: '"' | "'" | "`" | null = null;
  for (let i = openIndex; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (ch === "\\") {
        i++; // skip escaped char
        continue;
      }
      if (ch === inString) inString = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      inString = ch;
      continue;
    }
    if (ch === "(") depth++;
    else if (ch === ")") {
      depth--;
      if (depth === 0) return text.slice(openIndex, i + 1);
    }
  }
  throw new Error(`unbalanced parens in .mutate( call at index ${openIndex}`);
}

interface Violation {
  file: string;
  line: number;
  snippet: string;
  reason: "onSuccess-without-onError" | "neither-and-not-allowlisted";
}

const ALLOW_MARKER = "mutation-feedback-allow:";

/** True if the line just above, or the call's own line, carries the marker. */
function isAllowlisted(text: string, callIndex: number): boolean {
  const before = text.slice(0, callIndex);
  const lines = before.split("\n");
  const ownLineSoFar = lines[lines.length - 1];
  const priorLine = lines[lines.length - 2] ?? "";
  return ownLineSoFar.includes(ALLOW_MARKER) || priorLine.includes(ALLOW_MARKER);
}

function findViolations(file: string): Violation[] {
  const text = readFileSync(file, "utf8");
  const violations: Violation[] = [];
  const mutateRe = /\.mutate(Async)?\(/g;
  let match: RegExpExecArray | null;
  while ((match = mutateRe.exec(text)) !== null) {
    const openIndex = match.index + match[0].length - 1;
    const snippet = extractBalanced(text, openIndex);
    const line = text.slice(0, match.index).split("\n").length;
    const hasOnSuccess = snippet.includes("onSuccess");
    const hasOnError = snippet.includes("onError");
    if (hasOnSuccess && !hasOnError) {
      violations.push({ file, line, snippet: snippet.slice(0, 80), reason: "onSuccess-without-onError" });
    } else if (!hasOnSuccess && !hasOnError && !isAllowlisted(text, match.index)) {
      violations.push({ file, line, snippet: snippet.slice(0, 80), reason: "neither-and-not-allowlisted" });
    }
  }
  return violations;
}

function tsxFilesIn(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...tsxFilesIn(full));
    else if (entry.name.endsWith(".tsx")) out.push(full);
  }
  return out;
}

describe("mutation feedback (C1 gate)", () => {
  // J-20: every tsx under src/, not just the two admin pages dirs — the
  // exact spots this widening targets (src/wallet/**,
  // src/components/dashboard/**) were previously invisible to this gate.
  const pageFiles = tsxFilesIn(path.join(pkgRoot, "src"));

  it("scanned a non-trivial number of files", () => {
    // Pins the file-discovery glob itself — an empty list would make every
    // other assertion in this file vacuously true.
    expect(pageFiles.length).toBeGreaterThanOrEqual(70);
  });

  it("every .mutate(/.mutateAsync( call with onSuccess also has onError, and every call with neither is explicitly allowlisted with a reason", () => {
    const allViolations = pageFiles.flatMap((f) =>
      findViolations(f).map((v) => ({ ...v, file: path.relative(pkgRoot, v.file) })),
    );
    if (allViolations.length > 0) {
      const report = allViolations
        .map((v) => `  ${v.file}:${v.line}  [${v.reason}]  ${v.snippet}...`)
        .join("\n");
      throw new Error(
        `${allViolations.length} .mutate(/.mutateAsync( call(s) leave the operator with no failure feedback and no "${ALLOW_MARKER}" justification:\n${report}`,
      );
    }
  });
});
