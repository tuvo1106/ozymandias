import { isMetricCardinalityList, isMetricTags } from "./cardinalityApi";

const list = { metrics: [{ name: "m", type: "gauge", series: 3 }], total: 1, truncated: false };
const tags = { metric: "m", type: null, series: 3, keys: [{ key: "env", series: 3, values: 2 }] };

describe("cardinality responses", () => {
  it("accept the documented shapes, including a null type", () => {
    expect(isMetricCardinalityList(list)).toBe(true);
    expect(isMetricCardinalityList({ ...list, metrics: [{ ...list.metrics[0], type: null }] })).toBe(true);
    expect(isMetricTags(tags)).toBe(true);
  });

  // A missing type is not a null type: null is ozyd saying it has no record.
  it.each([
    ["a missing type", { ...list, metrics: [{ name: "m", series: 3 }] }],
    ["a fractional count", { ...list, metrics: [{ name: "m", type: null, series: 1.5 }] }],
    ["a negative total", { ...list, total: -1 }],
    ["no truncated flag", { metrics: [], total: 0 }],
  ])("refuse a list with %s", (_, v) => {
    expect(isMetricCardinalityList(v)).toBe(false);
  });

  it.each([
    ["no series", { ...tags, series: undefined }],
    ["a key without values", { ...tags, keys: [{ key: "env", series: 3 }] }],
    ["a type that is a number", { ...tags, type: 1 }],
  ])("refuse tags with %s", (_, v) => {
    expect(isMetricTags(v)).toBe(false);
  });
});
