import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Widget } from "../../lib/dashboard";
import { fakeServer, monitor, type Reply } from "../monitors/testServer";
import { AlertValueWidget, MonitorSummaryWidget, ServiceSummaryWidget } from "./FeedWidgets";

afterEach(() => vi.unstubAllGlobals());

const layout = { x: 0, y: 0, w: 6, h: 3 };

function show(widget: Widget, replies: Record<string, Reply>) {
  const calls = fakeServer(replies);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const Comp = { monitor_summary: MonitorSummaryWidget, alert_value: AlertValueWidget, service_summary: ServiceSummaryWidget }[
    widget.type as "monitor_summary" | "alert_value" | "service_summary"
  ];
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Comp widget={widget} results={[]} />
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return calls;
}

const list = (...ms: ReturnType<typeof monitor>[]): Reply => ({ body: { monitors: ms } });

describe("MonitorSummaryWidget", () => {
  const triggered: Widget = {
    id: "t",
    type: "monitor_summary",
    title: "Triggered",
    layout,
    monitor_filter: { tag: "team:shop", status: ["ALERT", "WARN", "NO_DATA"] },
  };

  it("asks the server for the tag, keeps the statuses, and lists worst first", async () => {
    const calls = show(triggered, {
      "GET /api/v1/monitors": list(
        monitor({ id: 1, name: "warned", status: "WARN", groups: { WARN: 1 } }),
        monitor({ id: 2, name: "fine", status: "OK", groups: { OK: 1 } }),
        monitor({ id: 3, name: "red", status: "ALERT", groups: { ALERT: 2 } }),
        monitor({ id: 4, name: "new", status: null, groups: {} }),
      ),
    });
    await screen.findByRole("link", { name: "red" });
    const items = within(screen.getByRole("list")).getAllByRole("listitem").map((li) => within(li).getByRole("link").textContent);
    expect(items).toEqual(["red", "warned"]);
    expect(calls[0]!.search.get("tag")).toBe("team:shop");
    expect(calls[0]!.search.has("status")).toBe(false);
    expect(screen.getByRole("link", { name: "red" })).toHaveAttribute("href", "/monitors/3");
  });

  it("says nothing is triggered when monitors match and none is in the statuses", async () => {
    show(triggered, { "GET /api/v1/monitors": list(monitor({ status: "OK", groups: { OK: 1 } })) });
    expect(await screen.findByText("Nothing is alerting, warning, without data.")).toBeInTheDocument();
  });

  it("says no monitor matches when the filter matches none at all", async () => {
    show(triggered, { "GET /api/v1/monitors": list() });
    expect(await screen.findByText("No monitor matches this widget's filter.")).toBeInTheDocument();
  });

  it("counts what is past the limit", async () => {
    show({ ...triggered, limit: 1 }, {
      "GET /api/v1/monitors": list(monitor({ id: 1, name: "a", status: "ALERT" }), monitor({ id: 2, name: "b", status: "ALERT" })),
    });
    expect(await screen.findByText(/and 1 more/)).toBeInTheDocument();
  });

  it("shows the error, not an empty list, when the list cannot be fetched", async () => {
    show(triggered, { "GET /api/v1/monitors": { status: 500, body: { error: "boom" } } });
    expect(await screen.findByText(/boom/)).toBeInTheDocument();
    expect(screen.queryByText(/Nothing is/)).not.toBeInTheDocument();
  });
});

describe("AlertValueWidget", () => {
  const w: Widget = { id: "a", type: "alert_value", layout, monitor_uid: "jobs" };

  it("shows the worst group's value and the monitor's status", async () => {
    show(w, {
      "GET /api/v1/monitors": list(monitor({ id: 7, uid: "jobs", name: "Jobs", status: "ALERT" })),
      "GET /api/v1/monitors/7/states": {
        body: {
          states: [
            { group: "queue:default", tags: { queue: "default" }, status: "ALERT", since: "2026-10-19T10:00:00Z", value: 31, last_eval: null, last_data: null, last_notified: null },
            { group: "queue:low", tags: { queue: "low" }, status: "OK", since: "2026-10-19T10:00:00Z", value: 2, last_eval: null, last_data: null, last_notified: null },
          ],
        },
      },
    });
    expect(await screen.findByText("31")).toBeInTheDocument();
    expect(screen.getByText("queue:default")).toBeInTheDocument();
    expect(screen.getByText("ALERT")).toBeInTheDocument();
  });

  it("says when no monitor has the uid, rather than drawing a blank", async () => {
    show({ ...w, monitor_uid: "gone" }, { "GET /api/v1/monitors": list(monitor({ uid: "jobs" })) });
    expect(await screen.findByText('No monitor has the uid "gone".')).toBeInTheDocument();
  });

  it("draws a dash, not a zero, for a monitor with no value yet", async () => {
    show(w, {
      "GET /api/v1/monitors": list(monitor({ id: 7, uid: "jobs", status: null, groups: {} })),
      "GET /api/v1/monitors/7/states": { body: { states: [] } },
    });
    expect(await screen.findByText("no data yet")).toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByText("0")).not.toBeInTheDocument();
  });
});

describe("ServiceSummaryWidget", () => {
  const w: Widget = { id: "s", type: "service_summary", layout, env: "prod" };
  const row = (service: string, extra: Record<string, unknown> = {}) => ({
    service,
    name: "http.request",
    requests: 600,
    requests_per_second: 2,
    errors: 6,
    error_pct: 1,
    p50_ms: 10,
    p95_ms: 120,
    p99_ms: 300,
    sparkline: [1, 2, 3],
    ...extra,
  });

  it("draws a card per service with its numbers and its monitors' worst status", async () => {
    const calls = show(w, {
      "GET /api/v1/services": { body: { services: [row("api"), row("worker", { p95_ms: null })], from: 1, to: 2 } },
      "GET /api/v1/monitors": list(
        monitor({ id: 1, status: "ALERT", tags: ["service:api"] }),
        monitor({ id: 2, status: "OK", tags: ["service:api"] }),
      ),
    });
    const api = (await screen.findByRole("link", { name: "api" })).closest("li")!;
    expect(api).toHaveTextContent("2.00");
    expect(api).toHaveTextContent("1.0%");
    expect(api).toHaveTextContent("120 ms");
    expect(within(api).getByTitle("Monitors: ALERT")).toBeInTheDocument();
    const worker = screen.getByRole("link", { name: "worker" }).closest("li")!;
    // No latency recorded is "—", not 0 ms; no tagged monitor is said, not green.
    expect(worker).toHaveTextContent("—");
    expect(worker).toHaveTextContent("no monitors");
    const svc = calls.find((c) => c.path === "/api/v1/services")!;
    expect(svc.search.get("env")).toBe("prod");
  });

  it("says no service handled a request, rather than drawing nothing", async () => {
    show(w, { "GET /api/v1/services": { body: { services: [], from: 1, to: 2 } }, "GET /api/v1/monitors": list() });
    expect(await screen.findByText(/No service handled a request in this window in env “prod”/)).toBeInTheDocument();
  });
});
