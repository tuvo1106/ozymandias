import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { Widget } from "../../lib/dashboard";
import type { TimeRange } from "../../lib/timeRange";
import { fakeServer, monitor, type Reply } from "../monitors/testServer";
import { AlertValueWidget, MonitorSummaryWidget, ServiceSummaryWidget } from "./FeedWidgets";

afterEach(() => vi.unstubAllGlobals());

const layout = { x: 0, y: 0, w: 6, h: 3 };
let queryClient: QueryClient;

function show(widget: Widget, replies: Record<string, Reply>, range?: TimeRange) {
  const calls = fakeServer(replies);
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  queryClient = client;
  const Comp = { monitor_summary: MonitorSummaryWidget, alert_value: AlertValueWidget, service_summary: ServiceSummaryWidget }[
    widget.type as "monitor_summary" | "alert_value" | "service_summary"
  ];
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <Comp widget={widget} results={[]} range={range} />
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

  it("keeps the tag and statuses from the shared list, and lists worst first", async () => {
    const calls = show(triggered, {
      "GET /api/v1/monitors": list(
        monitor({ id: 9, name: "other team", status: "ALERT", tags: ["team:other"] }),
        monitor({ id: 1, name: "warned", status: "WARN", groups: { WARN: 1 } }),
        monitor({ id: 2, name: "fine", status: "OK", groups: { OK: 1 } }),
        monitor({ id: 3, name: "red", status: "ALERT", groups: { ALERT: 2 } }),
        monitor({ id: 4, name: "new", status: null, groups: {} }),
      ),
    });
    await screen.findByRole("link", { name: "red" });
    const items = within(screen.getByRole("list")).getAllByRole("listitem").map((li) => within(li).getByRole("link").textContent);
    expect(items).toEqual(["red", "warned"]);
    // One list for every feed widget on the page: filtered here, not by the server.
    expect(calls.every((c) => c.path === "/api/v1/monitors" && c.search.get("states") === "1" && !c.search.has("tag"))).toBe(true);
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
    const href = new URL(screen.getByRole("link", { name: "all of them" }).getAttribute("href")!, "http://x");
    expect(href.pathname).toBe("/monitors");
    expect(href.searchParams.get("tag")).toBe("team:shop");
  });

  it("says a failed refresh over the last answer, rather than showing it as current", async () => {
    let n = 0;
    show(triggered, {
      "GET /api/v1/monitors": () => (n++ === 0 ? { body: { monitors: [monitor({ status: "OK", groups: { OK: 1 } })] } } : { status: 500, body: { error: "db locked" } }),
    });
    await screen.findByText("Nothing is alerting, warning, without data.");
    await queryClient.refetchQueries();
    expect(await screen.findByText(/Could not refresh the monitors: .*db locked.*Showing the last answer/)).toBeInTheDocument();
    expect(screen.getByText("Nothing is alerting, warning, without data.")).toBeInTheDocument();
  });

  it("shows the error, not an empty list, when the list cannot be fetched", async () => {
    show(triggered, { "GET /api/v1/monitors": { status: 500, body: { error: "boom" } } });
    expect(await screen.findByText(/boom/)).toBeInTheDocument();
    expect(screen.queryByText(/Nothing is/)).not.toBeInTheDocument();
  });
});

describe("AlertValueWidget", () => {
  const w: Widget = { id: "a", type: "alert_value", layout, monitor_uid: "jobs" };

  it("shows the worst group's value and the monitor's status, from one list request", async () => {
    const calls = show(w, {
      "GET /api/v1/monitors": list(
        monitor({
          id: 7,
          uid: "jobs",
          name: "Jobs",
          status: "ALERT",
          group_states: [
            { group: "queue:default", tags: { queue: "default" }, status: "ALERT", value: 31 },
            { group: "queue:low", tags: { queue: "low" }, status: "OK", value: 2 },
          ],
        }),
      ),
    });
    expect(await screen.findByText("31")).toBeInTheDocument();
    expect(screen.getByText("queue:default")).toBeInTheDocument();
    expect(screen.getByText("ALERT")).toBeInTheDocument();
    expect(calls.every((c) => c.path === "/api/v1/monitors" && c.search.get("states") === "1")).toBe(true);
  });

  it("says when no monitor has the uid, rather than drawing a blank", async () => {
    show({ ...w, monitor_uid: "gone" }, { "GET /api/v1/monitors": list(monitor({ uid: "jobs" })) });
    expect(await screen.findByText('No monitor has the uid "gone".')).toBeInTheDocument();
  });

  it("draws a dash, not a zero, for a monitor with no value yet", async () => {
    show(w, {
      "GET /api/v1/monitors": list(monitor({ id: 7, uid: "jobs", status: null, groups: {} })),
    });
    expect(await screen.findByText("no data yet")).toBeInTheDocument();
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.queryByText("0")).not.toBeInTheDocument();
  });

  it("says its status is as of now on a fixed past window", async () => {
    const past: TimeRange = { kind: "absolute", from: 1_700_000_000, to: 1_700_003_600 };
    show(w, {
      "GET /api/v1/monitors": list(monitor({ id: 7, uid: "jobs", status: "OK" })),
    }, past);
    expect(await screen.findByText("Monitor status is as of now, not the chosen window.")).toBeInTheDocument();
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

  const grp = (status: string, tags: Record<string, string>) => ({
    group: Object.entries(tags).map(([k, v]) => `${k}:${v}`).join(",") || "*",
    tags,
    status,
    value: 1,
  });

  it("draws a card per service with its numbers and its own env's worst status", async () => {
    const calls = show({ ...w, env: undefined }, {
      "GET /api/v1/services": {
        body: { services: [row("api", { env: "prod" }), row("api", { env: "staging", requests: 10 }), row("worker", { p95_ms: null })], from: 1, to: 2 },
      },
      "GET /api/v1/monitors": list(
        monitor({ id: 1, status: "ALERT", tags: ["service:api"], group_states: [grp("ALERT", { env: "staging" }), grp("OK", { env: "prod" })] }),
      ),
    });
    await screen.findByTitle("Monitors: ALERT");
    const [prod, staging] = screen.getAllByRole("link", { name: "api" }).map((a) => a.closest("li")!);
    expect(prod).toHaveTextContent("2.00");
    expect(prod).toHaveTextContent("1.0%");
    expect(prod).toHaveTextContent("120 ms");
    // A staging alert does not turn the prod card red.
    expect(within(prod!).getByTitle("Monitors: OK")).toBeInTheDocument();
    expect(within(staging!).getByTitle("Monitors: ALERT")).toBeInTheDocument();
    // The card opens the service in its env and the widget's window.
    const href = new URL(within(prod!).getByRole("link", { name: "api" }).getAttribute("href")!, "http://x");
    expect(href.pathname).toBe("/apm/services/api");
    expect(href.searchParams.get("env")).toBe("prod");
    const worker = screen.getByRole("link", { name: "worker" }).closest("li")!;
    // No latency recorded is "—", not 0 ms; no tagged monitor is said, not green.
    expect(worker).toHaveTextContent("—");
    expect(worker).toHaveTextContent("no monitors");
    expect(calls.some((c) => c.path === "/api/v1/services" && c.search.get("env") === null)).toBe(true);
  });

  it("asks the services table for its env", async () => {
    const calls = show(w, { "GET /api/v1/services": { body: { services: [row("api")], from: 1, to: 2 } }, "GET /api/v1/monitors": list() });
    await screen.findByRole("link", { name: "api" });
    expect(calls.find((c) => c.path === "/api/v1/services")!.search.get("env")).toBe("prod");
  });

  it("says the monitor statuses failed, rather than dropping the dots silently", async () => {
    show(w, {
      "GET /api/v1/services": { body: { services: [row("api")], from: 1, to: 2 } },
      "GET /api/v1/monitors": { status: 500, body: { error: "boom" } },
    });
    expect(await screen.findByText(/Monitor statuses could not be loaded/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("no monitors")).not.toBeInTheDocument());
  });

  it("polls a relative window and not a fixed one, whose answer cannot change", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const svc = { "GET /api/v1/services": { body: { services: [row("api")], from: 1, to: 2 } }, "GET /api/v1/monitors": list() };
      const fixed = show(w, svc, { kind: "absolute", from: 1_700_000_000, to: 1_700_003_600 });
      await screen.findByRole("link", { name: "api" });
      await vi.advanceTimersByTimeAsync(130_000);
      expect(fixed.filter((c) => c.path === "/api/v1/services")).toHaveLength(1);
      cleanup();
      const live = show(w, svc, { kind: "relative", preset: "1h" });
      await screen.findByRole("link", { name: "api" });
      await vi.advanceTimersByTimeAsync(130_000);
      expect(live.filter((c) => c.path === "/api/v1/services").length).toBeGreaterThan(1);
    } finally {
      vi.useRealTimers();
    }
  });

  it("says no service handled a request, rather than drawing nothing", async () => {
    show(w, { "GET /api/v1/services": { body: { services: [], from: 1, to: 2 } }, "GET /api/v1/monitors": list() });
    expect(await screen.findByText(/No service handled a request in this window in env “prod”/)).toBeInTheDocument();
  });
});
