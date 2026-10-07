import { expect, test } from "@playwright/test";

// Needs `scripts/seed-traces.py` to have posted traces (make e2e does): shop-api requests,
// some failing, and POST /api/orders ones that hand a job to billing-worker through a queue.

test("search a trace, open it, select a span, read its logs tab", async ({ page }) => {
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));
  page.on("console", (m) => m.type() === "error" && errors.push(m.text()));

  await page.goto("/apm/traces?env=dev&service=billing-worker");
  // Traces are queryable as soon as the agent flushes; poll rather than sleep.
  const first = page.locator('a[href^="/apm/traces/"]').first();
  await expect(async () => {
    await page.reload();
    await expect(first).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 60_000 });
  await first.click();

  await expect(page).toHaveURL(/\/apm\/traces\/[0-9a-f]{32}/);
  await expect(page.getByText(/5 spans/)).toBeVisible();
  await expect(page.getByText(/billing-worker, shop-api/)).toBeVisible();

  // The waterfall lists the cross-service hop; selecting the worker span shows its tags,
  // including the queue wait the agent-less SDK recorded.
  await page.getByRole("button", { name: "Waterfall" }).click();
  await page.getByRole("button", { name: /arq\.job/ }).first().click();
  await expect(page.getByText("billing-worker").first()).toBeVisible();
  await expect(page.getByText("queue.wait_ms")).toBeVisible();
  await expect(page.getByText("consumer")).toBeVisible();

  await page.getByRole("button", { name: "Logs" }).click();
  const panel = page.getByRole("region", { name: "Trace logs" });
  await expect(panel).toBeVisible();
  await expect(page).toHaveURL(/tab=logs/);
  await expect(panel.getByRole("link")).toHaveAttribute("href", /\/logs\?q=trace_id/);

  expect(errors).toEqual([]);
});

test("services table and service map show the seeded services", async ({ page }) => {
  await page.goto("/apm?env=dev");
  // RED numbers come from metrics, which lag the traces by an aggregation interval.
  await expect(async () => {
    await page.reload();
    await expect(page.getByRole("link", { name: "shop-api" })).toBeVisible({ timeout: 2_000 });
  }).toPass({ timeout: 90_000 });
  await expect(page.getByRole("link", { name: "billing-worker" })).toBeVisible();

  await page.getByRole("link", { name: "Service map" }).click();
  await expect(page).toHaveURL(/\/apm\/map/);
  await expect(page.locator("svg, canvas").first()).toBeVisible();
});
