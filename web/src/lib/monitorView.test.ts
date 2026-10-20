import { describe, expect, it } from "vitest";
import type { GroupState, MonitorEvent } from "./monitorsApi";
import { describeEvent, muteLabel, statusTimeline } from "./monitorView";

const iso = (s: number) => new Date(s * 1000).toISOString();
const tr = (ts: number, group: string, from: string, to: string): MonitorEvent =>
  ({ id: ts, ts: iso(ts), kind: "transition", monitor_id: 1, group, from, to, value: 1 }) as MonitorEvent;
const st = (group: string, status: string, since: number): GroupState =>
  ({ group, tags: {}, status, since: iso(since), value: 1, last_eval: null, last_data: null, last_notified: null }) as GroupState;

describe("muteLabel", () => {
  it("reads a mute", () => {
    expect(muteLabel(null, 100)).toBeNull();
    expect(muteLabel(100, 100)).toBeNull(); // ended
    expect(muteLabel(100 + 3600 * 2 + 300, 100)).toBe("muted for 2h 5m");
    expect(muteLabel(Date.UTC(9999, 0, 1) / 1000, 100)).toBe("muted until unmuted");
  });
});

describe("statusTimeline", () => {
  it("reads transitions backwards from the current state", () => {
    const out = statusTimeline([tr(400, "*", "ALERT", "OK"), tr(200, "*", "OK", "ALERT")], [st("*", "OK", 400)], 0, 1000);
    expect(out).toEqual([
      { group: "*", segments: [
        { from: 0, to: 200, status: "OK" },
        { from: 200, to: 400, status: "ALERT" },
        { from: 400, to: 1000, status: "OK" },
      ] },
    ]);
  });

  it("is one segment for a group that did not change", () => {
    expect(statusTimeline([], [st("*", "ALERT", -50)], 0, 1000)[0]!.segments).toEqual([{ from: 0, to: 1000, status: "ALERT" }]);
  });

  // A group created part-way through was not OK before it existed.
  it("is unknown before a group's state began", () => {
    expect(statusTimeline([], [st("*", "OK", 600)], 0, 1000)[0]!.segments).toEqual([
      { from: 0, to: 600, status: null },
      { from: 600, to: 1000, status: "OK" },
    ]);
  });

  it("is unknown before a reset, and ignores transitions from before it", () => {
    const events = [{ id: 9, ts: iso(500), kind: "reset", monitor_id: 1, value: null } as MonitorEvent, tr(300, "*", "OK", "ALERT"), tr(700, "*", "OK", "WARN")];
    expect(statusTimeline(events, [st("*", "WARN", 700)], 0, 1000)[0]!.segments).toEqual([
      { from: 0, to: 500, status: null },
      { from: 500, to: 700, status: "OK" },
      { from: 700, to: 1000, status: "WARN" },
    ]);
  });

  it("keeps a group known only from its events, after the current ones", () => {
    const out = statusTimeline([tr(300, "q:gone", "OK", "NO_DATA")], [st("q:a", "OK", -1)], 0, 1000);
    expect(out.map((g) => g.group)).toEqual(["q:a", "q:gone"]);
    expect(out[1]!.segments).toEqual([
      { from: 0, to: 300, status: "OK" },
      { from: 300, to: 1000, status: "NO_DATA" },
    ]);
  });

  it("ignores transitions outside the range", () => {
    expect(statusTimeline([tr(2000, "*", "OK", "ALERT")], [st("*", "OK", -1)], 0, 1000)[0]!.segments).toEqual([
      { from: 0, to: 1000, status: "OK" },
    ]);
  });
});

describe("describeEvent", () => {
  it("reads each kind", () => {
    const e = (kind: string, extra: Partial<MonitorEvent> = {}) => describeEvent({ id: 1, ts: "", kind, monitor_id: 1, value: null, ...extra });
    expect(e("transition", { from: "OK", to: "ALERT" })).toBe("OK → ALERT");
    expect(e("notify_failed", { text: "hook: HTTP 500" })).toBe("delivery failed: hook: HTTP 500");
    expect(e("reset")).toBe("query changed; group states cleared");
    expect(e("something_new", { text: "x" })).toBe("something_new: x");
  });
});
