/**
 * Vite + Vitest configuration for the ozymandias web UI.
 *
 * - `build` writes straight into the Go embed directory
 *   (`internal/api/ui/dist`), so `make web && make build` produces a single
 *   binary that serves the UI. Only `dist/.gitkeep` is tracked; emptyOutDir
 *   deletes it, so the npm `build` script recreates it after Vite runs.
 * - The dev server (:9401) proxies API and health routes to an ozyd on
 *   :9400, so `make dev` gives hot reload against a real backend.
 * - Coverage is gated on `src/lib/**` — the pure logic. Components are covered
 *   by behaviour tests and, from M3, Playwright (docs/plan/testing.md L11).
 */
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import { defineConfig } from "vitest/config";

const backend = process.env.OZY_URL ?? "http://localhost:9400";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    outDir: "../internal/api/ui/dist",
    emptyOutDir: true,
  },
  server: {
    port: 9401,
    strictPort: true,
    proxy: {
      "/api": backend,
      "/v1": backend,
      "/healthz": backend,
      "/debug": backend,
    },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    coverage: {
      provider: "v8",
      include: ["src/lib/**"],
      thresholds: { lines: 70, branches: 70, functions: 70, statements: 70 },
    },
  },
});
