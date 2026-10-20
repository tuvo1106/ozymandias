/**
 * A fake ozyd for the monitor page tests: replies keyed by "METHOD /path",
 * every request recorded with its parsed body.
 */
import { vi } from "vitest";

/** One canned answer. A function gets the request body. */
export type Reply = { status?: number; body: unknown } | ((body: unknown, url: URL) => { status?: number; body: unknown });

/** A request the page made. */
export interface Call {
  method: string;
  path: string;
  search: URLSearchParams;
  body: unknown;
}

/** Stubs fetch; returns the calls made so far, live. */
export function fakeServer(replies: Record<string, Reply>): Call[] {
  const calls: Call[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: string, init?: RequestInit) => {
      const url = new URL(input, "http://localhost");
      const method = init?.method ?? "GET";
      const body = typeof init?.body === "string" ? (JSON.parse(init.body) as unknown) : undefined;
      calls.push({ method, path: url.pathname, search: url.searchParams, body });
      const r = replies[`${method} ${url.pathname}`];
      const reply = typeof r === "function" ? r(body, url) : (r ?? { status: 404, body: { error: `no route for ${method} ${url.pathname}` } });
      const status = reply.status ?? 200;
      return new Response(status === 204 ? null : JSON.stringify(reply.body), { status });
    }),
  );
  return calls;
}

/** A stored monitor with every field, overridable. */
export function monitor(extra: Record<string, unknown> = {}) {
  return {
    id: 1,
    uid: "jobs",
    name: "Job queue is backing up",
    type: "metric",
    query: "avg(last_5m):avg:jobs.queue.depth{*} by {queue} > 20",
    options: { thresholds: { warning: 10 } },
    message: "Queue {{.Value}} @hook",
    tags: ["team:shop"],
    enabled: true,
    provisioned: false,
    created_at: "2026-10-19T10:00:00Z",
    updated_at: "2026-10-19T10:00:00Z",
    muted_until: null,
    groups: { ALERT: 1 },
    status: "ALERT",
    ...extra,
  };
}

/** A valid validate answer. */
export const validAnswer = {
  valid: true,
  query: "avg(last_5m):avg:jobs.queue.depth{*} by {queue} > 20",
  grouped: true,
  levels: { op: ">", critical: 20, critical_recovery: 15, warning: 10, warning_recovery: 10 },
  mentions: ["hook"],
  window: { from: 1790000000, to: 1790000300 },
  interval_s: 30,
  groups: [{ group: "queue:a", tags: { queue: "a" }, value: 12, status: "WARN" }],
};

/** A backtest answer with one transition. */
export const backtestAnswer = {
  from: "2026-10-18T10:00:00Z",
  to: "2026-10-19T10:00:00Z",
  evaluations: 2,
  events: [{ ts: "2026-10-19T09:00:00Z", group: "queue:a", from: "OK", to: "ALERT", value: 25, notify: true, reason: "transition" }],
  final: { "queue:a": "ALERT" },
  series: [{ group: "queue:a", tags: { queue: "a" }, points: [[1790000000, 5], [1790000030, 25]] }],
  series_omitted: 0,
};
