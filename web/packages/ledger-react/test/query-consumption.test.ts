import { readFileSync, readdirSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";

/*
 * Static "query consumption completeness" gate (J-24, 2026-09-02 web audit).
 *
 * skin-parity.test.ts's static assertions can only ever catch drift BETWEEN
 * the two skins — by construction they are blind to a bug that both skins
 * carry identically. This audit's three Major findings (J-1, J-2, J-3) were
 * ALL exactly that shape: a query's `data` was consumed while its `isError`
 * (and often `isLoading`) was silently dropped, in both skins at once, so
 * skin-parity's "shadcn mirrors heroui" gate is structurally unable to fire.
 *
 * Rule enforced, skin-independent: for every `useXxx(...)` hook call bound to
 * either (a) a destructured `{ data, ... }` pattern, or (b) a plain
 * identifier later read as `<ident>.data`, this file's source must ALSO
 * consume that same binding's `isError` (destructured, or `<ident>.isError`)
 * SOMEWHERE — otherwise a failed request and a genuinely-empty result render
 * identically. A call site that legitimately doesn't need this (e.g. a
 * write-side mutation misidentified as a query, or a hook whose caller
 * handles errors through a different mechanism) opts out with a
 * same-line/prior-line `query-consumption-allow: <reason>` comment.
 *
 * "readFileSync + regex" in the style of the existing test/styles.test.ts,
 * test/mutation-feedback.test.ts and test/skin-parity.test.ts — no parser
 * needed for a heuristic this exact (audit's own suggested design).
 */

const pkgRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "..",
);

const ALLOW_MARKER = "query-consumption-allow:";

function tsxFilesIn(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) out.push(...tsxFilesIn(full));
    else if (entry.name.endsWith(".tsx")) out.push(full);
  }
  return out;
}

/** Extract the balanced-brace snippet starting at text[openIndex] === "{". */
function extractBalancedBraces(text: string, openIndex: number): string {
  let depth = 0;
  let inString: '"' | "'" | "`" | null = null;
  for (let i = openIndex; i < text.length; i++) {
    const ch = text[i];
    if (inString) {
      if (ch === "\\") {
        i++;
        continue;
      }
      if (ch === inString) inString = null;
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      inString = ch;
      continue;
    }
    if (ch === "{") depth++;
    else if (ch === "}") {
      depth--;
      if (depth === 0) return text.slice(openIndex, i + 1);
    }
  }
  throw new Error(`unbalanced braces at index ${openIndex}`);
}

/** Top-level (depth-0) destructured key names, e.g. "{ data: foo, isError }" -> ["data","isError"]. */
function destructuredKeys(braceSnippet: string): string[] {
  const inner = braceSnippet.slice(1, -1);
  const parts: string[] = [];
  let depth = 0;
  let current = "";
  for (const ch of inner) {
    if (ch === "{" || ch === "(" || ch === "[") depth++;
    else if (ch === "}" || ch === ")" || ch === "]") depth--;
    if (ch === "," && depth === 0) {
      parts.push(current);
      current = "";
    } else {
      current += ch;
    }
  }
  if (current.trim()) parts.push(current);
  return parts
    .map((p) => p.split(":")[0].trim())
    .filter((p) => p.length > 0 && !p.startsWith("..."));
}

function isAllowlisted(text: string, callIndex: number): boolean {
  const before = text.slice(0, callIndex);
  const lines = before.split("\n");
  const ownLineSoFar = lines[lines.length - 1];
  const priorLine = lines[lines.length - 2] ?? "";
  return ownLineSoFar.includes(ALLOW_MARKER) || priorLine.includes(ALLOW_MARKER);
}

interface Violation {
  file: string;
  line: number;
  detail: string;
}

function findViolations(file: string): Violation[] {
  const text = readFileSync(file, "utf8");
  const violations: Violation[] = [];

  // Case 1: destructured — `const { data: snapData, ... } = useXxx(`. The
  // object pattern's own content (arbitrary keys/aliases/nesting) sits
  // between `{` and the closing `}`, so this must be matched in two steps:
  // find `const {`, extract the balanced brace group, THEN check what
  // immediately follows it is `= useXxx(`.
  const constBraceRe = /const\s*\{/g;
  let braceMatch: RegExpExecArray | null;
  while ((braceMatch = constBraceRe.exec(text)) !== null) {
    const braceStart = braceMatch.index + braceMatch[0].indexOf("{");
    const snippet = extractBalancedBraces(text, braceStart);
    const rest = text.slice(braceStart + snippet.length);
    const afterMatch = /^\s*=\s*use[A-Z]\w*\(/.exec(rest);
    if (!afterMatch) continue;
    const keys = destructuredKeys(snippet);
    if (keys.includes("data") && !keys.includes("isError")) {
      if (!isAllowlisted(text, braceMatch.index)) {
        const line = text.slice(0, braceMatch.index).split("\n").length;
        violations.push({
          file,
          line,
          detail: `destructures data without isError: ${snippet.slice(0, 80)}`,
        });
      }
    }
  }

  // Case 2: whole-object binding — `const holds = useWalletHolds();`. Check
  // whether `.data` is read anywhere and, if so, whether `.isError` ever is
  // too, anywhere in the file (the binding may be threaded through several
  // components/functions before either is read).
  const identRe = /const\s+([A-Za-z_$][\w$]*)\s*=\s*use[A-Z]\w*\(/g;
  let identMatch: RegExpExecArray | null;
  while ((identMatch = identRe.exec(text)) !== null) {
    const ident = identMatch[1];
    const line = text.slice(0, identMatch.index).split("\n").length;
    const dataRe = new RegExp(`\\b${ident}\\.data\\b`);
    const isErrorRe = new RegExp(`\\b${ident}\\.isError\\b`);
    if (dataRe.test(text) && !isErrorRe.test(text)) {
      if (!isAllowlisted(text, identMatch.index)) {
        violations.push({
          file,
          line,
          detail: `'${ident}.data' is read but '${ident}.isError' never is`,
        });
      }
    }
  }

  return violations;
}

describe("query consumption completeness (J-24 gate)", () => {
  const files = tsxFilesIn(path.join(pkgRoot, "src"));

  it("scanned a non-trivial number of files", () => {
    expect(files.length).toBeGreaterThanOrEqual(70);
  });

  it("every useXxx() binding whose data is read also reads isError somewhere, or is explicitly allowlisted with a reason", () => {
    const allViolations = files.flatMap((f) =>
      findViolations(f).map((v) => ({ ...v, file: path.relative(pkgRoot, v.file) })),
    );
    if (allViolations.length > 0) {
      const report = allViolations
        .map((v) => `  ${v.file}:${v.line}  ${v.detail}`)
        .join("\n");
      throw new Error(
        `${allViolations.length} query binding(s) consume data without isError and carry no "${ALLOW_MARKER}" justification — a failed request and a genuinely-empty result would render identically:\n${report}`,
      );
    }
  });
});
