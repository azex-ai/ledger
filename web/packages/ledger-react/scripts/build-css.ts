/*
 * Compiles src/styles/index.css -> dist/styles.css using the Tailwind v4
 * PostCSS plugin. The input imports only Tailwind's theme + utilities layers
 * (no preflight), so the emitted CSS has no global element reset.
 *
 * Invoked from tsup's onSuccess (see tsup.config.ts) after dist/ is written.
 */
import { mkdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import postcss from "postcss";
import tailwindcss from "@tailwindcss/postcss";

const pkgRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

// input (relative to src/styles) -> output (relative to dist)
const SHEETS: Array<[string, string]> = [
  ["index.css", "styles.css"], // shadcn skin: tokens + scoped preflight + utilities
  ["heroui.css", "heroui.css"], // heroui skin: layout utilities only (host owns the HeroUI theme)
];

export async function buildCss(): Promise<string[]> {
  const out: string[] = [];
  for (const [input, output] of SHEETS) {
    const inputPath = path.join(pkgRoot, "src", "styles", input);
    const outPath = path.join(pkgRoot, "dist", output);
    const css = await readFile(inputPath, "utf8");
    const result = await postcss([
      // `base` is the directory @source globs resolve against.
      tailwindcss({ base: pkgRoot, optimize: { minify: true } }),
    ]).process(css, { from: inputPath, to: outPath });

    await mkdir(path.dirname(outPath), { recursive: true });
    await writeFile(outPath, result.css);
    out.push(outPath);
  }
  return out;
}
