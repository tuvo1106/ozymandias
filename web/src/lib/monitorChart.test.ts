import { describe, expect, it } from "vitest";
import { clipBand } from "./annotations";
import { backtestData, breachesAbove, levelAnnotations, STATUS_COLOR, transitionMarkers } from "./monitorChart";
import type { Backtest } from "./monitorsApi";

describe("levelAnnotations", () => {
  it("puts ALERT above critical and WARN between warning and critical for >", () => {
    const a = levelAnnotations({ op: ">", critical: 20, critical_recovery: 15, warning: 10, warning_recovery: 10 });
    const [alert, warn] = a.bands!;
    expect(clipBand(alert!.from, alert!.to, 0, 100)).toEqual([20, 100]);
    expect(clipBand(warn!.from, warn!.to, 0, 100)).toEqual([10, 20]);
    // The critical recovery differs from critical: a dashed line. The warning
    // recovery is the warning itself: no line on top of the band's edge.
    expect(a.hlines!.filter((l) => l.dash).map((l) => l.y)).toEqual([15]);
    expect(a.yInclude).toEqual([20, 10, 15]);
  });

  it("puts ALERT below critical for <", () => {
    const a = levelAnnotations({ op: "<", critical: 5, critical_recovery: 5, warning: 10, warning_recovery: 12 });
    const [alert, warn] = a.bands!;
    expect(clipBand(alert!.from, alert!.to, 0, 100)).toEqual([0, 5]);
    expect(clipBand(warn!.from, warn!.to, 0, 100)).toEqual([5, 10]);
    expect(a.hlines!.filter((l) => l.dash).map((l) => l.y)).toEqual([12]);
  });

  it("has one band without a warning", () => {
    const a = levelAnnotations({ op: ">=", critical: 1, critical_recovery: 1 });
    expect(a.bands).toHaveLength(1);
    expect(a.hlines).toHaveLength(1);
  });
});

describe("breachesAbove", () => {
  it("is the direction of the operator", () => {
    expect([">", ">=", "<", "<="].map((op) => breachesAbove(op as ">"))).toEqual([true, true, false, false]);
  });
});

describe("transitionMarkers", () => {
  it("marks transitions by the status entered, and nothing else", () => {
    const m = transitionMarkers([
      { ts: "2026-01-01T00:00:30Z", group: "*", from: "OK", to: "ALERT", value: 1, notify: true },
      { ts: "2026-01-01T00:01:00Z", group: "*", from: "ALERT", to: "ALERT", value: 1, notify: true, reason: "renotify" },
      { id: 1, ts: "2026-01-01T00:02:00Z", kind: "notify_ok", monitor_id: 1, value: null },
      { id: 2, ts: "2026-01-01T00:03:00Z", kind: "transition", monitor_id: 1, from: "ALERT", to: "OK", value: 0 },
    ]);
    expect(m).toEqual([
      { x: Date.UTC(2026, 0, 1, 0, 0, 30) / 1000, color: STATUS_COLOR.ALERT },
      { x: Date.UTC(2026, 0, 1, 0, 3, 0) / 1000, color: STATUS_COLOR.OK },
    ]);
  });
});

const bt = (series: Backtest["series"]): Backtest => ({
  from: "", to: "", evaluations: 2, events: [], final: {}, series, series_omitted: 0, transitions: 0, notifications: 0, events_omitted: 0,
});

describe("backtestData", () => {
  it("shares the x column when every line has the same ticks", () => {
    const { data, labels } = backtestData(bt([
      { group: "q:a", tags: {}, points: [[30, 1], [60, null]] },
      { group: "q:b", tags: {}, points: [[30, null], [60, 2]] },
    ]));
    expect(data).toEqual([[30, 60], [1, null], [null, 2]]);
    expect(labels).toEqual(["q:a", "q:b"]);
  });
  it("aligns by timestamp when lines disagree", () => {
    const { data } = backtestData(bt([
      { group: "a", tags: {}, points: [[30, 1]] },
      { group: "b", tags: {}, points: [[60, 2]] },
    ]));
    expect(data).toEqual([[30, 60], [1, null], [null, 2]]);
  });
  it("is empty with no lines", () => {
    expect(backtestData(bt([])).data).toEqual([[]]);
  });
});
