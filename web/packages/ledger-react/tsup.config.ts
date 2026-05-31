import { readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { defineConfig } from "tsup";

const DIRECTIVES = ["use client", "use server"] as const;
// Single source of truth: the regex alternation is derived from DIRECTIVES.
// NOTE: matches only when the directive is the FIRST statement of the file
// (optional whitespace allowed, but no comment/banner above it). RSC directives
// are invalid if anything precedes them, so a future build-banner change that
// injects a leading comment would silently defeat this hook.
const DIRECTIVE_RE = new RegExp(
  `^\\s*["'](${DIRECTIVES.join("|")})["']\\s*;?`,
);

interface Metafile {
  outputs: Record<string, { inputs: Record<string, unknown> }>;
}

/**
 * esbuild strips leading directives (e.g. "use client") when the directive
 * lives in an *imported* module rather than the bundle entry, which silently
 * breaks React Server Components boundaries. We run in `onSuccess`, after tsup
 * has written dist/ and the metafile, and rewrite the chunks in place: for each
 * output chunk we inspect every source it bundled, and if any source begins
 * with a directive, re-prepend it to the chunk.
 *
 * A chunk that pulls in any "use client" source keeps the directive; chunks
 * built only from undirected sources stay undirected. The metafile is deleted
 * afterward so it does not ship in the published tarball.
 */
async function preserveDirectives(distDir: string): Promise<void> {
  const metaPath = path.resolve(distDir, "metafile-esm.json");
  const meta = JSON.parse(await readFile(metaPath, "utf8")) as Metafile;
  const cwd = process.cwd();
  const directiveCache = new Map<string, string | null>();

  const directiveOf = async (input: string): Promise<string | null> => {
    const cached = directiveCache.get(input);
    if (cached !== undefined) return cached;
    let directive: string | null = null;
    try {
      const src = await readFile(path.resolve(cwd, input), "utf8");
      directive = DIRECTIVE_RE.exec(src)?.[1] ?? null;
    } catch {
      directive = null;
    }
    directiveCache.set(input, directive);
    return directive;
  };

  await Promise.all(
    Object.entries(meta.outputs).map(async ([outPath, output]) => {
      if (!outPath.endsWith(".js")) return;
      let directive: string | null = null;
      for (const input of Object.keys(output.inputs)) {
        directive = await directiveOf(input);
        if (directive) break;
      }
      if (!directive) return;
      const abs = path.resolve(cwd, outPath);
      const text = await readFile(abs, "utf8");
      if (text.startsWith(`"${directive}"`)) return;
      await writeFile(abs, `"${directive}";\n${text}`);
    }),
  );

  // Build-only artifact — keep it out of the published tarball.
  await rm(metaPath, { force: true });
}

export default defineConfig({
  entry: ["src/index.ts", "src/server.ts"],
  format: ["esm"],
  dts: true,
  clean: true,
  splitting: true,
  metafile: true,
  external: [
    "react",
    "react-dom",
    "react/jsx-runtime",
    "@tanstack/react-query",
  ],
  async onSuccess() {
    await preserveDirectives("dist");
  },
});
