import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

export default defineConfig({
  root: fileURLToPath(new URL("../", import.meta.url)),
  resolve: {
    alias: {
      "@": fileURLToPath(new URL("../src", import.meta.url)),
      // Next supplies this server-component marker in production. Tests run
      // the helper on Node and use Next's own empty server implementation.
      "server-only": fileURLToPath(new URL("../node_modules/next/dist/compiled/server-only/empty.js", import.meta.url)),
    },
  },
  test: {
    environment: "jsdom",
    include: ["test/**/*.test.tsx"],
  },
});
