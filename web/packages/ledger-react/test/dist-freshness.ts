import { readdirSync, readFileSync, statSync } from "node:fs";
import path from "node:path";

/*
 * m-4 (W3 adversarial review of the gates): test/styles.test.ts and
 * test/build-artifacts.test.ts read dist/. A MISSING dist throws, which is
 * fail-closed and correct. A STALE dist does not: those tests then assert
 * against the previous build, and `npm test` does not produce a dist, so the
 * only thing keeping them honest was web/CLAUDE.md's "build first"
 * convention. A dist built before the change under test is a gate reporting
 * on code that is not the code (working-agreements §3).
 *
 * readDistFile is the shared reader those two suites now use: it throws when
 * dist is missing, and throws when any dist artifact is older than the
 * newest file in src/, naming both so the message is actionable.
 */

const pkgRoot = path.resolve(path.dirname(new URL(import.meta.url).pathname), "..");
const distDir = path.join(pkgRoot, "dist");
const srcDir = path.join(pkgRoot, "src");

function newestFileIn(dir: string): { file: string; mtimeMs: number } {
  let newest = { file: "", mtimeMs: 0 };
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      const inner = newestFileIn(full);
      if (inner.mtimeMs > newest.mtimeMs) newest = inner;
      continue;
    }
    const { mtimeMs } = statSync(full);
    if (mtimeMs > newest.mtimeMs) newest = { file: full, mtimeMs };
  }
  return newest;
}

/**
 * Read a file out of dist/, failing when dist is absent OR stale.
 *
 * "Stale" is measured against src/: if any source file is newer than THIS
 * artifact, the artifact does not describe the code in the working tree.
 */
export function readDistFile(name: string): string {
  const target = path.join(distDir, name);
  let artifact: string;
  try {
    artifact = readFileSync(target, "utf8");
  } catch {
    throw new Error(`dist/${name} not found at ${target}. Run \`npm run build\` before tests.`);
  }
  const artifactMtime = statSync(target).mtimeMs;
  const newestSource = newestFileIn(srcDir);
  if (newestSource.mtimeMs > artifactMtime) {
    throw new Error(
      `dist/${name} is STALE: ${path.relative(pkgRoot, newestSource.file)} was modified after it was built ` +
        `(${new Date(newestSource.mtimeMs).toISOString()} > ${new Date(artifactMtime).toISOString()}). ` +
        `This suite would be asserting against the previous build, which is a gate reporting on code that is not the code. ` +
        `Run \`npm run build\` before tests (web/CLAUDE.md), or delete dist/ to get the missing-artifact error instead.`,
    );
  }
  return artifact;
}
