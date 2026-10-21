import { expect, test, type APIRequestContext } from "@playwright/test";

// The M6 L11 path: create a monitor in the editor, push a gauge over its threshold, and
// see the detail page go to ALERT and back to OK; then delete it through the page.
// Needs ozyd with monitors enabled (make e2e runs deploy/ozyd.yaml, which has them).

const run = `e2e${Date.now()}`;
const metric = "e2e.monitor.gauge";

async function push(request: APIRequestContext, value: number) {
  const res = await request.post("http://localhost:9400/v1/series", {
    data: { series: [{ metric, type: "gauge", interval: 0, tags: [`run:${run}`], points: [[Math.floor(Date.now() / 1000), value]] }] },
  });
  expect(res.ok()).toBe(true);
}

/** Pushes `value` every 5s until the status pill in the header says `status`. */
async function driveUntil(page: import("@playwright/test").Page, request: APIRequestContext, value: number, status: string) {
  const pill = page.locator("header").getByText(status, { exact: true }).first();
  await expect(async () => {
    await push(request, value);
    await page.reload();
    await expect(pill).toBeVisible({ timeout: 5_000 });
  }).toPass({ timeout: 120_000, intervals: [5_000] });
}

test("create a monitor, trigger it, see it alert and recover", async ({ page, request }) => {
  test.setTimeout(300_000);
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(e.message));

  await push(request, 1);
  await page.goto("/monitors/new");
  await page.getByLabel("Name").fill(`e2e monitor ${run}`);
  await page.getByLabel("Query").fill(`last(last_1m):avg:${metric}{run:${run}} > 5`);
  await page.getByLabel("Evaluation interval").fill("10");
  await page.getByLabel("Evaluation delay").fill("10");
  // The preview answers before anything is saved.
  await expect(page.getByText(/Reads as last\(last_1m\)/)).toBeVisible({ timeout: 15_000 });
  await page.getByRole("button", { name: "Create monitor" }).click();
  await expect(page).toHaveURL(/\/monitors\/\d+$/);

  await driveUntil(page, request, 10, "ALERT");
  await expect(page.getByRole("region", { name: "History" })).toContainText("OK → ALERT");
  await driveUntil(page, request, 0, "OK");

  await page.getByRole("button", { name: "Delete", exact: true }).click();
  await page.getByRole("button", { name: "Delete, with its history" }).click();
  await expect(page).toHaveURL(/\/monitors$/);
  await expect(page.getByText(`e2e monitor ${run}`)).toHaveCount(0);
  expect(errors).toEqual([]);
});
