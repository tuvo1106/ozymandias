import { render, screen, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { fakeServer } from "../pages/monitors/testServer";
import { routes } from "./routes";
import { triggered } from "./MonitorStatus";

const status = (extra: Record<string, unknown>) => ({ running: true, monitors: 3, stalled: false, loading: false, last_eval_age_seconds: 1, monitors_by_status: {}, ...extra });

function renderWith(body: unknown, code = 200) {
  fakeServer({ "GET /api/v1/monitors/status": { status: code, body }, "GET /healthz": { body: { status: "ok", component: "ozyd", version: "v", uptime_seconds: 1 } } });
  render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: ["/"] })} />);
}

afterEach(() => vi.unstubAllGlobals());

describe("monitor status in the shell", () => {
  it("counts ALERT and NO_DATA monitors on the nav item", async () => {
    renderWith(status({ monitors_by_status: { ALERT: 2, NO_DATA: 1, WARN: 4, OK: 9 } }));
    const nav = screen.getByRole("navigation", { name: "Main" });
    expect(await within(nav).findByLabelText("3 triggered")).toHaveTextContent("3");
  });

  it("shows no badge when nothing is triggered", async () => {
    renderWith(status({ monitors_by_status: { WARN: 1, OK: 2 } }));
    await screen.findByText(/ozyd v is up/);
    expect(screen.queryByLabelText(/triggered/)).toBeNull();
    expect(screen.queryByRole("alert")).toBeNull(); // running: no banner
  });

  it("puts a banner on every page when evaluation has stopped", async () => {
    renderWith(status({ stalled: true, last_eval_age_seconds: 330 }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Monitoring is down: no monitor has completed an evaluation for 5m 30s");
  });

  it("says when the scheduler cannot read its monitors", async () => {
    renderWith(status({ stalled: true, loading: true }));
    expect(await screen.findByRole("alert")).toHaveTextContent("cannot read its monitors");
  });

  // ozyd unreachable is the home page's to say; a banner of "unknown" on
  // every page would be noise.
  it("shows nothing when the status cannot be read", async () => {
    renderWith({ error: "boom" }, 500);
    await screen.findByText(/ozyd v is up/);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

describe("triggered", () => {
  it("is ALERT plus NO_DATA", () => {
    expect(triggered({ ALERT: 1, NO_DATA: 2, WARN: 5 })).toBe(3);
    expect(triggered({})).toBe(0);
  });
});
