import { describe, expect, it, vi } from "vitest";

// uPlot reads matchMedia when it loads; jsdom has none.
vi.stubGlobal("matchMedia", () => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
const { buildOptions } = await import("./TimeseriesChart");

const opts = (yInclude?: number[]) =>
  buildOptions(["a"], false, 600, 300, { current: undefined }, undefined, { current: { yInclude } }, (yInclude?.length ?? 0) > 0);

describe("buildOptions", () => {
  // Every other chart keeps uPlot's own y range; only one that asks to
  // include a value gets the widened one.
  it("overrides the y range only for a chart that includes values", () => {
    expect(opts().scales?.y).toBeUndefined();
    const range = opts([20]).scales?.y?.range as (u: unknown, min: number, max: number) => [number, number];
    expect(range(undefined, 4, 6)[1]).toBeGreaterThan(20);
  });
});
