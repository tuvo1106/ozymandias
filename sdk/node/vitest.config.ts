import { defineConfig } from "vitest/config";

// Coverage gate: 90% lines/branches/functions/statements for the SDK
// (docs/plan/testing.md: SDKs are critical packages).
export default defineConfig({
  test: {
    include: ["test/**/*.test.ts"],
    environment: "node",
    // The contract and safety tests bind real UDP sockets on 127.0.0.1 and
    // mutate process-wide state (globalThis, process listeners, env); running
    // files one at a time keeps them independent.
    fileParallelism: false,
    coverage: {
      provider: "v8",
      include: ["src/**/*.ts"],
      reporter: ["text", "text-summary"],
      thresholds: { lines: 90, branches: 90, functions: 90, statements: 90 },
    },
  },
});
