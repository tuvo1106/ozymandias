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

  // The list grows a milestone at a time, and a section going live is a
  // deliberate act — so it is written down here rather than inferred, and
  // turning a flag on without meaning to fails.
  it("has Metrics, Dashboards and Logs live, and everything else still coming", () => {
    expect(NAV_ITEMS.filter((i) => i.live).map((i) => i.label)).toEqual(["Metrics", "Dashboards", "Logs"]);
  });
});
