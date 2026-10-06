import { describe, expect, it } from "vitest";
import { columns } from "./ServiceMap";

describe("service map columns", () => {
  const e = (parent: string, child: string) => ({ parent, child });
  it("puts callers left of callees, by the longest chain", () => {
    const c = columns(["a", "b", "c", "d"], [e("a", "b"), e("b", "c"), e("a", "c"), e("a", "d")]);
    expect([c.get("a"), c.get("b"), c.get("c"), c.get("d")]).toEqual([0, 1, 2, 1]);
  });
  it("terminates on a cycle and on a self-call", () => {
    const c = columns(["a", "b"], [e("a", "b"), e("b", "a"), e("a", "a")]);
    expect([...c.values()].every((v) => v < 2)).toBe(true);
  });
  it("leaves unconnected services in the first column", () => {
    expect(columns(["x"], []).get("x")).toBe(0);
  });
});
