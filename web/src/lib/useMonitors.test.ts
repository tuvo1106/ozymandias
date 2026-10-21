import { describe, expect, it } from "vitest";
import { isAboutMonitor, MONITORS_KEY } from "./useMonitors";

describe("isAboutMonitor", () => {
  it("matches a monitor's row, states, events and backtest by position", () => {
    expect(isAboutMonitor([MONITORS_KEY, "one", 7], 7)).toBe(true);
    expect(isAboutMonitor([MONITORS_KEY, "states", 7], 7)).toBe(true);
    expect(isAboutMonitor([MONITORS_KEY, "events", { monitor_id: 7, limit: 1000 }, 86400], 7)).toBe(true);
    expect(isAboutMonitor([MONITORS_KEY, "backtest", "{}", 7], 7)).toBe(true);
  });

  it("does not match a number in another place, or another monitor", () => {
    expect(isAboutMonitor([MONITORS_KEY, "events", { monitor_id: 1 }, 86400], 86400)).toBe(false);
    expect(isAboutMonitor([MONITORS_KEY, "one", 8], 7)).toBe(false);
    expect(isAboutMonitor([MONITORS_KEY, "list", {}], 7)).toBe(false);
    expect(isAboutMonitor(["dashboards", "one", 7], 7)).toBe(false);
  });
});
