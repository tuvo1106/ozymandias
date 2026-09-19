import { findNavItem, NAV_ITEMS } from "./nav";

describe("findNavItem", () => {
  it("matches a section's own path and anything beneath it", () => {
    expect(findNavItem("/logs")?.label).toBe("Logs");
    expect(findNavItem("/logs/live/tail")?.label).toBe("Logs");
  });

  it("matches whole segments only", () => {
    expect(findNavItem("/logsearch")).toBeUndefined();
    expect(findNavItem("/")).toBeUndefined();
  });
});

describe("NAV_ITEMS", () => {
  it("has unique, absolute paths and a milestone for every section", () => {
    const paths = NAV_ITEMS.map((i) => i.path);
    expect(new Set(paths).size).toBe(paths.length);
    for (const item of NAV_ITEMS) {
      expect(item.path).toMatch(/^\/[a-z]+$/);
      expect(item.milestone).toMatch(/^M\d$/);
    }
  });
});
