import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  // Override default ignores of eslint-config-next.
  globalIgnores([
    // Default ignores of eslint-config-next:
    ".next/**",
    "out/**",
    "build/**",
    "next-env.d.ts",
    // The workspace package is a separate project with its own lint/test
    // toolchain (and ships built artifacts in dist/). The app must not lint
    // into it — it consumes it as a dependency.
    "packages/**",
  ]),
]);

export default eslintConfig;
