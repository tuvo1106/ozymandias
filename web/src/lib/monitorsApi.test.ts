import { describe, expect, it } from "vitest";
import { ApiError } from "./metricsApi";
import {
  backtestMonitor,
  deleteMonitor,
  fetchEvents,
  fetchMonitor,
  fetchMonitors,
  fetchMonitorsStatus,
  fetchNotifiers,
  fetchStates,
  muteMonitor,
  severity,
  testNotification,
  validateMonitor,
  type MonitorDefinition,
} from "./monitorsApi";

function fakeFetch(body: unknown, status = 200) {
  const calls: { url: string; init?: RequestInit }[] = [];
  const f = ((url: string, init?: RequestInit) => {
    calls.push({ url, init });
    return Promise.resolve(new Response(status === 204 ? null : JSON.stringify(body), { status }));
  }) as unknown as typeof fetch;
  return { f, calls };
}

const stored = {
  id: 1, uid: "jobs-backlog", name: "Job queue is backing up", type: "metric",
  query: "avg(last_5m):avg:jobs.queue.depth{*} by {queue} > 20",
  options: { thresholds: { warning: 10 } }, message: "Queue {{.Value}} @hook", tags: ["team:shop"],
  enabled: true, provisioned: false, created_at: "2026-09-21T14:13:20Z", updated_at: "2026-09-21T14:13:20Z",
  muted_until: null, groups: { ALERT: 1 }, status: "ALERT",
};

const def: MonitorDefinition = { name: "x", type: "metric", query: "avg(last_5m):avg:x{*} > 1", options: {}, message: "", tags: [] };

describe("fetchMonitors", () => {
  it("passes only the filters that are set", async () => {
    const { f, calls } = fakeFetch({ monitors: [stored] });
    await expect(fetchMonitors({ tag: "team:shop", q: "" }, f)).resolves.toEqual([stored]);
    expect(calls[0]?.url).toBe("/api/v1/monitors?tag=team%3Ashop");
  });

  // null is "unknown" (no groups, disabled, or a problem), and must survive the
  // shape check rather than being treated as a broken row.
  it("accepts a null status and a problem", async () => {
    const row = { ...stored, status: null, groups: {}, problem: "query: bad" };
    const { f } = fakeFetch({ monitors: [row] });
    await expect(fetchMonitors({}, f)).resolves.toEqual([row]);
  });

  it("refuses a row that is not a monitor", async () => {
    const { f } = fakeFetch({ monitors: [{ ...stored, status: "PURPLE" }] });
    await expect(fetchMonitors({}, f)).rejects.toBeInstanceOf(ApiError);
  });
});

describe("one monitor", () => {
  it("fetches by id and reports the server's 404 sentence", async () => {
    const ok = fakeFetch(stored);
    await expect(fetchMonitor(1, ok.f)).resolves.toEqual(stored);
    const missing = fakeFetch({ error: "no monitor with id 9" }, 404);
    await expect(fetchMonitor(9, missing.f)).rejects.toThrow("no monitor with id 9");
  });

  it("deletes with a DELETE and an empty answer", async () => {
    const { f, calls } = fakeFetch(null, 204);
    await expect(deleteMonitor(3, f)).resolves.toBeUndefined();
    expect(calls[0]?.init?.method).toBe("DELETE");
  });

  // No body means "until unmuted"; the server reads an empty object the same way.
  it("mutes with or without an end", async () => {
    const { f, calls } = fakeFetch({ ...stored, muted_until: 1790003600 });
    await muteMonitor(1, 1790003600, f);
    await muteMonitor(1, undefined, f);
    expect(calls.map((c) => c.init?.body)).toEqual(['{"until":1790003600}', "{}"]);
  });
});

describe("states and events", () => {
  it("reads states", async () => {
    const st = { group: "queue:a", tags: { queue: "a" }, status: "ALERT", since: "t", value: 25, last_eval: "t", last_data: null, last_notified: null };
    const { f } = fakeFetch({ states: [st] });
    await expect(fetchStates(1, f)).resolves.toEqual([st]);
  });

  it("builds the event query", async () => {
    const { f, calls } = fakeFetch({ events: [] });
    await fetchEvents({ monitor_id: 2, from: 10.7, to: 20.2, kind: ["transition", "renotify"], limit: 50 }, f);
    expect(calls[0]?.url).toBe("/api/v1/events?monitor_id=2&from=10&to=21&kind=transition%2Crenotify&limit=50");
  });
});

describe("validate", () => {
  // "Invalid" is validate's successful answer, not a rejection.
  it("resolves with the reasons for an invalid monitor", async () => {
    const { f } = fakeFetch({ valid: false, errors: ["a", "b"] });
    await expect(validateMonitor(def, f)).resolves.toEqual({ valid: false, errors: ["a", "b"] });
  });

  it("reads a valid answer and refuses one missing its window", async () => {
    const ok = { valid: true, query: "q", grouped: false, levels: { op: ">", critical: 1, critical_recovery: 1 }, mentions: [], window: { from: 1, to: 2 }, interval_s: 30 };
    await expect(validateMonitor(def, fakeFetch(ok).f)).resolves.toEqual(ok);
    const old: Record<string, unknown> = { ...ok };
    delete old.window;
    await expect(validateMonitor(def, fakeFetch(old).f)).rejects.toBeInstanceOf(ApiError);
  });
});

describe("backtest", () => {
  it("sends the monitor with the saved id and reads the lines", async () => {
    const body = { from: "f", to: "t", evaluations: 2, events: [], final: {}, series: [{ group: "*", tags: {}, points: [[1, null], [2, 3]] }], series_omitted: 0, transitions: 0, notifications: 0, events_omitted: 0 };
    const { f, calls } = fakeFetch(body);
    await expect(backtestMonitor(def, { id: 4 }, f)).resolves.toEqual(body);
    expect(JSON.parse(calls[0]!.init!.body as string)).toEqual({ monitor: def, id: 4 });
  });

  it("refuses a point that is not a pair", async () => {
    const body = { evaluations: 1, events: [], final: {}, series: [{ group: "*", tags: {}, points: [[1]] }], series_omitted: 0, transitions: 0, notifications: 0, events_omitted: 0 };
    await expect(backtestMonitor(def, {}, fakeFetch(body).f)).rejects.toBeInstanceOf(ApiError);
  });
});

describe("status, notifiers, test", () => {
  it("reads the status document", async () => {
    const s = { running: true, monitors: 1, stalled: false, loading: false, last_eval_age_seconds: 0, monitors_by_status: { ALERT: 1 } };
    await expect(fetchMonitorsStatus(fakeFetch(s).f)).resolves.toEqual(s);
  });

  it("keeps only well-formed notifiers and results", async () => {
    const n = { name: "hook", type: "webhook", ready: true };
    await expect(fetchNotifiers(fakeFetch({ notifiers: [n, { name: 1 }] }).f)).resolves.toEqual([n]);
    const r = { notifier: "hook", ok: false, error: "HTTP 500" };
    await expect(testNotification(1, fakeFetch({ results: [r, {}] }).f)).resolves.toEqual([r]);
  });
});

describe("severity", () => {
  it("orders unknown below OK and ALERT worst", () => {
    expect([null, "OK", "WARN", "NO_DATA", "ALERT"].map((s) => severity(s as "OK"))).toEqual([-1, 0, 1, 2, 3]);
  });
});
