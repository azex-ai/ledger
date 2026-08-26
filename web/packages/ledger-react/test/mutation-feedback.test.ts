import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Static gate for C1 (2026-08-26 web audit) — "money-path mutation failures
 * are completely silent." A `.mutate(...)` call that hands `useMutation` an
 * `onSuccess` but no `onError` is a silent-failure bug by construction: the
 * button stops spinning, the dialog closes on click (AlertDialogAction), and
 * nothing tells the operator the write failed (nextjs.md: "沉默 = bug").
 *
 * Rule enforced: for every `.mutate(` call site in src/{components,heroui}/
 * pages/*.tsx, if its (paren-balanced) argument list contains `onSuccess`, it
 * must also contain `onError` in that SAME argument list. Calls with neither
 * (e.g. ReconciliationPage's bare `mutation.mutate()`, fed back via inline
 * `.isError` rendering instead of a toast) are a legitimate, different
 * pattern and are not required to add onSuccess/onError at all — this gate
 * only fires once a call site has already opted into the onSuccess-toast
 * pattern without its onError counterpart.
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
}

function findViolations(file: string): Violation[] {
  const text = readFileSync(file, "utf8");
  const violations: Violation[] = [];
  const mutateRe = /\.mutate\(/g;
  let match: RegExpExecArray | null;
  while ((match = mutateRe.exec(text)) !== null) {
    const openIndex = match.index + match[0].length - 1;
    const snippet = extractBalanced(text, openIndex);
    if (snippet.includes("onSuccess") && !snippet.includes("onError")) {
      const line = text.slice(0, match.index).split("\n").length;
      violations.push({ file, line, snippet: snippet.slice(0, 80) });
    }
  }
  return violations;
}

function tsxFilesIn(dir: string): string[] {
  return readdirSync(dir)
    .filter((f) => f.endsWith(".tsx"))
    .map((f) => path.join(dir, f));
}

describe("mutation feedback (C1 gate)", () => {
  const pageFiles = [
    ...tsxFilesIn(path.join(pkgRoot, "src/components/pages")),
    ...tsxFilesIn(path.join(pkgRoot, "src/heroui/pages")),
  ];

  it("scanned a non-trivial number of page files", () => {
    // Pins the file-discovery glob itself — an empty list would make every
    // other assertion in this file vacuously true.
    expect(pageFiles.length).toBeGreaterThanOrEqual(28);
  });

  it("every .mutate( call with onSuccess also has onError", () => {
    const allViolations = pageFiles.flatMap((f) =>
      findViolations(f).map((v) => ({ ...v, file: path.relative(pkgRoot, v.file) })),
    );
    if (allViolations.length > 0) {
      const report = allViolations
        .map((v) => `  ${v.file}:${v.line}  ${v.snippet}...`)
        .join("\n");
      throw new Error(
        `${allViolations.length} .mutate( call(s) pass onSuccess without a sibling onError — the operator gets no failure feedback:\n${report}`,
      );
    }
  });
});
