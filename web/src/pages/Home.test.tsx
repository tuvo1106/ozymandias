import { render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../app/routes";
import { fakeServer, monitor, type Reply } from "./monitors/testServer";

afterEach(() => vi.unstubAllGlobals());

const health = { body: { status: "ok", component: "ozyd", version: "v0.1.0", uptime_seconds: 200 } };
const monitorsStatus = { body: { running: true, monitors: 1, stalled: false, loading: false, last_eval_age_seconds: 1, monitors_by_status: {} } };

function renderHome(replies: Record<string, Reply>) {
  // The dashboard loads the chart library, which reads matchMedia; jsdom has none.
  vi.stubGlobal("matchMedia", () => ({ matches: false, addEventListener() {}, removeEventListener() {} }));
  const calls = fakeServer({ "GET /api/v1/health": health, "GET /healthz": health, "GET /api/v1/monitors/status": monitorsStatus, ...replies });
  render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: ["/"] })} />);
  return calls;
}

const home = {
  id: 1,
  uid: "home",
  title: "Home",
  provisioned: true,
  created_at: "2026-10-24T00:00:00Z",
  updated_at: "2026-10-24T00:00:00Z",
  widgets: [
    {
      id: "triggered",
      type: "monitor_summary",
      title: "Triggered monitors",
      layout: { x: 0, y: 0, w: 6, h: 3 },
      monitor_filter: { status: ["ALERT", "WARN", "NO_DATA"] },
    },
  ],
};

describe("Home", () => {
  it("shows the provisioned home dashboard, with its widgets drawing", async () => {
    renderHome({
      "GET /api/v1/dashboards": { body: { dashboards: [{ ...home, id: 2, uid: "other", title: "Other" }, home], unreadable: [] } },
      "GET /api/v1/monitors": { body: { monitors: [monitor({ id: 3, name: "queue is backing up", status: "ALERT" })] } },
    });
    expect(await screen.findByRole("link", { name: "queue is backing up" })).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Home" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Other" })).not.toBeInTheDocument();
  });

  it("says when there is no home dashboard, and keeps the sections", async () => {
    renderHome({ "GET /api/v1/dashboards": { body: { dashboards: [{ ...home, uid: "other", title: "Other" }], unreadable: [] } } });
    expect(await screen.findByText(/No dashboard has the uid "home"/)).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: "Sections" })).toBeInTheDocument();
  });

  it("says when the dashboards cannot be listed, rather than claiming there is no home", async () => {
    renderHome({ "GET /api/v1/dashboards": { status: 500, body: { status: "error", error: "db locked" } } });
    expect(await screen.findByText(/could not be listed/)).toBeInTheDocument();
    expect(screen.queryByText(/No dashboard has the uid/)).not.toBeInTheDocument();
  });
});
