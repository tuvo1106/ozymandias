import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../app/routes";
import type { ChartAnnotations } from "../../lib/annotations";
import { STATUS_COLOR } from "../../lib/monitorChart";
import { backtestAnswer, fakeServer, monitor, validAnswer, type Reply } from "./testServer";

const drawn: { annotations?: ChartAnnotations }[] = [];
vi.mock("../../charts/TimeseriesChart", () => ({
  TimeseriesChart: (p: { labels: string[]; annotations?: ChartAnnotations }) => {
    drawn.push(p);
    return <div data-testid="chart">{p.labels.join(" | ")}</div>;
  },
}));

const status = { running: true, monitors: 1, stalled: false, loading: false, last_eval_age_seconds: 1, monitors_by_status: {} };
const hourAgo = new Date(Date.now() - 3600_000).toISOString();
const state = { group: "queue:a", tags: { queue: "a" }, status: "ALERT", since: hourAgo, value: 25, last_eval: hourAgo, last_data: hourAgo, last_notified: hourAgo };
const events = [
  { id: 3, ts: hourAgo, kind: "notify_failed", monitor_id: 1, group: "queue:a", value: null, text: "hook: HTTP 500" },
  { id: 2, ts: hourAgo, kind: "notify_ok", monitor_id: 1, group: "queue:a", value: null, text: "ops" },
  { id: 1, ts: hourAgo, kind: "transition", monitor_id: 1, group: "queue:a", from: "OK", to: "ALERT", value: 25 },
];

function renderAt(path: string, replies: Record<string, Reply> = {}) {
  const calls = fakeServer({
    "GET /api/v1/monitors/status": { body: status },
    "GET /api/v1/monitors/1": { body: monitor() },
    "GET /api/v1/monitors/1/states": { body: { states: [state] } },
    "GET /api/v1/events": { body: { events } },
    "POST /api/v1/monitors/validate": { body: validAnswer },
    "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    ...replies,
  });
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return { calls, router };
}

afterEach(() => {
  vi.unstubAllGlobals();
  drawn.length = 0;
});

describe("MonitorPage", () => {
  it("shows the groups, the timeline, the notification log and the history", async () => {
    const { calls } = renderAt("/monitors/1");
    const groups = await screen.findByRole("region", { name: "Groups" });
    expect(await within(groups).findByText("queue:a")).toBeInTheDocument();
    const log = screen.getByRole("region", { name: "Notification log" });
    await waitFor(() => expect(within(log).getAllByRole("row")).toHaveLength(2));
    expect(log).toHaveTextContent("delivery failed: hook: HTTP 500");
    expect(within(screen.getByRole("region", { name: "History" })).getAllByRole("row")).toHaveLength(3);
    // The timeline: unknown before the state began? No: a transition explains it.
    expect(screen.getByRole("img", { name: "OK, ALERT" })).toBeInTheDocument();
    // The events asked for are this monitor's, over the last day.
    const asked = calls.find((c) => c.path === "/api/v1/events")!.search;
    expect(asked.get("monitor_id")).toBe("1");
    expect(Number(asked.get("to")) - Number(asked.get("from"))).toBeGreaterThanOrEqual(86_399);
  });

  // The chart replays the last day; the markers are what was recorded, not
  // what the replay found, so a difference between them is visible.
  it("marks the recorded transitions over the replayed line", async () => {
    renderAt("/monitors/1");
    await screen.findByTestId("chart");
    await waitFor(() => expect(drawn.at(-1)!.annotations!.markers).toEqual([{ x: Date.parse(hourAgo) / 1000, color: STATUS_COLOR.ALERT }]));
  });

  it("does not validate or replay a monitor that is not running", async () => {
    const { calls } = renderAt("/monitors/1", { "GET /api/v1/monitors/1": { body: monitor({ status: null, groups: {}, problem: "query: bad" }) } });
    expect(await screen.findByText("Not running: query: bad")).toBeInTheDocument();
    await screen.findByRole("region", { name: "History" });
    expect(calls.some((c) => c.path.endsWith("/validate") || c.path.endsWith("/backtest"))).toBe(false);
  });

  it("sends a test and shows each notifier's answer", async () => {
    const user = userEvent.setup();
    renderAt("/monitors/1", {
      "POST /api/v1/monitors/1/test-notification": { body: { results: [{ notifier: "hook", ok: true }, { notifier: "ops", ok: false, error: "HTTP 500" }] } },
    });
    await user.click(await screen.findByRole("button", { name: "Test notification" }));
    const results = await screen.findByRole("list", { name: "Test results" });
    expect(results).toHaveTextContent("@hook: sent");
    expect(results).toHaveTextContent("@ops: HTTP 500");
  });

  it("deletes only on the second click", async () => {
    const user = userEvent.setup();
    const { calls, router } = renderAt("/monitors/1", {
      "DELETE /api/v1/monitors/1": { status: 204, body: null },
      "GET /api/v1/monitors": { body: { monitors: [] } },
    });
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(calls.some((c) => c.method === "DELETE")).toBe(false);
    await user.click(screen.getByRole("button", { name: "Delete, with its history" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/monitors"));
    expect(calls.filter((c) => c.method === "DELETE")).toHaveLength(1);
  });

  it("offers no delete for a provisioned monitor", async () => {
    renderAt("/monitors/1", { "GET /api/v1/monitors/1": { body: monitor({ provisioned: true }) } });
    expect(await screen.findByRole("link", { name: "View definition" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete" })).toBeNull();
  });

  it("mutes until unmuted and says so", async () => {
    const user = userEvent.setup();
    let muted: number | null = null;
    const { calls } = renderAt("/monitors/1", {
      "GET /api/v1/monitors/1": () => ({ body: monitor({ muted_until: muted }) }),
      "POST /api/v1/monitors/1/mute": () => {
        muted = Date.UTC(9999, 0, 1) / 1000;
        return { body: monitor({ muted_until: muted }) };
      },
    });
    await user.click(await screen.findByRole("button", { name: "Mute" }));
    expect(await screen.findByText(/muted until unmuted/)).toBeInTheDocument();
    expect(calls.find((c) => c.path.endsWith("/mute"))!.body).toEqual({});
  });
});
