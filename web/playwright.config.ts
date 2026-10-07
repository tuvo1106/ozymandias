import { defineConfig } from "@playwright/test";

/**
 * Browser end-to-end tests (ADR-0046). They drive the UI against a running stack that
 * `make e2e` brings up and seeds; they are not part of `make ci` or the Actions gate, because
 * they need a browser binary and a live ozyd + agent. `OZY_E2E_URL` points at a UI that proxies
 * `/api` (ozyd itself on :9400, or Vite on :9401).
 */
export default defineConfig({
  testDir: "e2e",
  testMatch: "**/*.e2e.ts",
  timeout: 90_000,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  use: { baseURL: process.env.OZY_E2E_URL ?? "http://localhost:9401", trace: "retain-on-failure" },
});
