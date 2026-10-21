import { describe, expect, it } from "vitest";
import { clipBand, inXRange, yRangeIncluding } from "./annotations";

describe("clipBand", () => {
  it("clamps an infinite edge to the visible range", () => {
    expect(clipBand(20, Infinity, 0, 50)).toEqual([20, 50]);
    expect(clipBand(20, -Infinity, 0, 50)).toEqual([0, 20]);
  });
  it("accepts its edges in either order", () => {
    expect(clipBand(30, 10, 0, 50)).toEqual([10, 30]);
  });
  it("is null when the band is off screen or has no height", () => {
    expect(clipBand(60, Infinity, 0, 50)).toBeNull();
    expect(clipBand(-Infinity, -5, 0, 50)).toBeNull();
    expect(clipBand(10, 10, 0, 50)).toBeNull();
  });
});

describe("yRangeIncluding", () => {
  // The case the editor exists for: a line well below its threshold.
  it("stretches to a threshold far from the data, padded", () => {
    const [lo, hi] = yRangeIncluding(4, 6, [20]);
    expect(lo).toBeCloseTo(4 - 0.8);
    expect(hi).toBeCloseTo(20 + 0.8);
  });
  it("uses only the included values with no data", () => {
    expect(yRangeIncluding(null, null, [10, 20])).toEqual([9.5, 20.5]);
  });
  it("gives a flat or empty range some height", () => {
    expect(yRangeIncluding(5, 5)).toEqual([3.9, 6.1]);
    expect(yRangeIncluding(undefined, undefined)).toEqual([0, 1]);
  });
  it("ignores infinities", () => {
    expect(yRangeIncluding(0, 10, [Infinity, -Infinity])).toEqual([-0.5, 10.5]);
  });
});

describe("inXRange", () => {
  it("includes its ends", () => {
    expect(inXRange(10, 10, 20)).toBe(true);
    expect(inXRange(20, 10, 20)).toBe(true);
    expect(inXRange(21, 10, 20)).toBe(false);
  });
});
