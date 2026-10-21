import { onlineManager } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../app/routes";
import type { ChartAnnotations } from "../../lib/annotations";
import { STATUS_COLOR } from "../../lib/monitorChart";
import { VALIDATE_DEBOUNCE_MS } from "./MonitorEditor";
import { backtestAnswer, fakeServer, monitor, validAnswer, type Reply } from "./testServer";

// The chart records what it was asked to draw, which is what these tests are about.
const drawn: { labels: string[]; annotations?: ChartAnnotations }[] = [];
vi.mock("../../charts/TimeseriesChart", () => ({
  TimeseriesChart: (p: { labels: string[]; annotations?: ChartAnnotations }) => {
    drawn.push(p);
    return <div data-testid="chart">{p.labels.join(" | ")}</div>;
  },
}));

const status = { running: true, monitors: 1, stalled: false, loading: false, last_eval_age_seconds: 1, monitors_by_status: {} };

function renderAt(path: string, replies: Record<string, Reply>) {
  const calls = fakeServer({
    "GET /api/v1/monitors/status": { body: status },
    "GET /api/v1/notifiers": { body: { notifiers: [{ name: "hook", type: "webhook", ready: true }] } },
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

describe("MonitorEditor", () => {
  it("draws the backtest's line with the levels' bands, the window and the markers", async () => {
    const { calls } = renderAt("/monitors/new", {
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    expect(await screen.findByTestId("chart")).toHaveTextContent("queue:a");
    const a = drawn.at(-1)!.annotations!;
    expect(a.bands).toHaveLength(2); // ALERT above 20, WARN 10..20
    expect(a.yInclude).toEqual([20, 10, 15]);
    expect(a.spans).toEqual([expect.objectContaining({ from: 1790000000, to: 1790000300 })]);
    expect(a.markers).toEqual([{ x: Date.parse("2026-10-19T09:00:00Z") / 1000, color: STATUS_COLOR.ALERT }]);
    expect(screen.getByText(/changed status 1 time\(s\) and notified 1 time\(s\)/)).toBeInTheDocument();
    expect(screen.getByText(/notifies @hook/)).toBeInTheDocument();
    // The backtest replays what was validated.
    const v = calls.find((c) => c.path.endsWith("/validate"))!.body;
    expect(calls.find((c) => c.path.endsWith("/backtest"))!.body).toEqual({ monitor: v });
  });

  it("shows every reason the server gives, and does not backtest an invalid draft", async () => {
    const { calls } = renderAt("/monitors/new", {
      "POST /api/v1/monitors/validate": { body: { valid: false, errors: ["query: unknown aggregator \"avgg\"", "@nobody names no notifier"] } },
    });
    const problems = await screen.findByRole("list", { name: "Problems" });
    expect(within(problems).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "query: unknown aggregator \"avgg\"",
      "@nobody names no notifier",
    ]);
    expect(calls.some((c) => c.path.endsWith("/backtest"))).toBe(false);
  });

  // Shallow checks happen before asking anyone; nothing is sent for them.
  it("names a box that is not a number without asking the server", async () => {
    const user = userEvent.setup();
    const { calls } = renderAt("/monitors/new", { "POST /api/v1/monitors/validate": { body: validAnswer }, "POST /api/v1/monitors/backtest": { body: backtestAnswer } });
    await screen.findByTestId("chart"); // the starting draft was asked about
    const asked = calls.filter((c) => c.path.endsWith("/validate")).length;
    await user.type(screen.getByLabelText("Warning"), "ten");
    expect(await screen.findByText("warning must be a number")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create monitor" })).toBeDisabled();
    // Past the debounce: still nothing new sent.
    await new Promise((r) => setTimeout(r, VALIDATE_DEBOUNCE_MS + 200));
    expect(calls.filter((c) => c.path.endsWith("/validate"))).toHaveLength(asked);
  });

  it("creates and opens the new monitor", async () => {
    const user = userEvent.setup();
    const { calls, router } = renderAt("/monitors/new", {
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
      "POST /api/v1/monitors": (b) => ({ status: 201, body: monitor({ ...(b as object), id: 7 }) }),
      "GET /api/v1/monitors/7": { body: monitor({ id: 7 }) },
      "GET /api/v1/monitors/7/states": { body: { states: [] } },
      "GET /api/v1/events": { body: { events: [] } },
    });
    await user.type(await screen.findByLabelText("Name"), "Backlog");
    await user.click(screen.getByRole("button", { name: "Create monitor" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/monitors/7"));
    expect(calls.find((c) => c.method === "POST" && c.path === "/api/v1/monitors")!.body).toMatchObject({ name: "Backlog", type: "metric" });
  });

  it("shows why a save was refused", async () => {
    const user = userEvent.setup();
    renderAt("/monitors/new", {
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
      "POST /api/v1/monitors": { status: 409, body: { error: "uid \"jobs\" is taken" } },
    });
    await user.type(await screen.findByLabelText("Name"), "x");
    await user.click(screen.getByRole("button", { name: "Create monitor" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("uid \"jobs\" is taken");
  });

  it("opens a stored monitor with its values, and a provisioned one read-only", async () => {
    renderAt("/monitors/3/edit", {
      "GET /api/v1/monitors/3": { body: monitor({ id: 3, provisioned: true, options: { thresholds: { warning: 10 }, renotify_interval_m: 60 } }) },
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    expect(await screen.findByLabelText("Warning")).toHaveValue("10");
    expect(screen.getByLabelText("Renotify interval")).toHaveValue("60");
    expect(screen.getByText(/provisioned from a file, so it is read-only/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    // Still read-only once edited: the file is where it changes.
    await userEvent.setup().type(screen.getByLabelText("Warning"), "1");
    expect(screen.getByText(/unsaved changes/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("passes the saved id to the backtest, so its ticks fall where the live ones do", async () => {
    const { calls } = renderAt("/monitors/3/edit", {
      "GET /api/v1/monitors/3": { body: monitor({ id: 3 }) },
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    await screen.findByTestId("chart");
    expect(calls.find((c) => c.path.endsWith("/backtest"))!.body).toMatchObject({ id: 3 });
  });
});

describe("EditMonitorPage", () => {
  // The detail page caches the monitor; someone saves in between; Edit must
  // start from the server's copy, not the cached one.
  it("starts the draft from a fresh read, not the cache", async () => {
    const user = userEvent.setup();
    let warning = 10;
    const { router } = renderAt("/monitors/3", {
      "GET /api/v1/monitors/3": () => ({ body: monitor({ id: 3, options: { thresholds: { warning } } }) }),
      "GET /api/v1/monitors/3/states": { body: { states: [] } },
      "GET /api/v1/events": { body: { events: [] } },
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    await screen.findByRole("link", { name: "Edit" });
    warning = 12;
    await user.click(screen.getByRole("link", { name: "Edit" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/monitors/3/edit"));
    expect(await screen.findByLabelText("Warning")).toHaveValue("12");
  });

  // A refetch after the draft started (a reconnect, a save's invalidation)
  // that fails must not swap the editor for the error.
  it("keeps the draft when a later read of the monitor fails", { timeout: 15_000 }, async () => {
    const user = userEvent.setup();
    let fail = false;
    const { calls } = renderAt("/monitors/3/edit", {
      "GET /api/v1/monitors/3": () => (fail ? { status: 500, body: { error: "database is locked" } } : { body: monitor({ id: 3 }) }),
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    await user.type(await screen.findByLabelText("Renotify interval"), "45");
    fail = true;
    // Stale (staleTime is 5s), so the reconnect refetches it, and it fails.
    vi.useFakeTimers({ toFake: ["Date"] });
    vi.setSystemTime(Date.now() + 60_000);
    try {
      onlineManager.setOnline(false);
      onlineManager.setOnline(true); // refetchOnReconnect
      // The first read, then the refetch and its two retries (a 5xx is retried).
      await waitFor(() => expect(calls.filter((c) => c.path === "/api/v1/monitors/3").length).toBe(4), { timeout: 8000 });
      await new Promise((r) => setTimeout(r, 300));
    } finally {
      vi.useRealTimers();
    }
    expect(screen.queryByText(/Could not read the monitor/)).toBeNull();
    expect(screen.getByLabelText("Renotify interval")).toHaveValue("45");
  });

  // Same page, another id, with that monitor still fresh in the cache: it
  // must still be read again, not wait for a fetch that never comes.
  it("opens another monitor's editor from a fresh cache", async () => {
    const { router } = renderAt("/monitors/4", {
      "GET /api/v1/monitors/3": { body: monitor({ id: 3, name: "three" }) },
      "GET /api/v1/monitors/4": { body: monitor({ id: 4, name: "four" }) },
      "GET /api/v1/monitors/4/states": { body: { states: [] } },
      "GET /api/v1/events": { body: { events: [] } },
      "POST /api/v1/monitors/validate": { body: validAnswer },
      "POST /api/v1/monitors/backtest": { body: backtestAnswer },
    });
    await screen.findByRole("heading", { name: "four" });
    await router.navigate("/monitors/3/edit");
    expect(await screen.findByLabelText("Name")).toHaveValue("three");
    await router.navigate("/monitors/4/edit");
    await waitFor(() => expect(screen.getByLabelText("Name")).toHaveValue("four"));
  });
});
