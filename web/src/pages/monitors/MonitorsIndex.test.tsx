import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../app/routes";
import { fakeServer, monitor, type Reply } from "./testServer";

const status = { running: true, monitors: 1, stalled: false, loading: false, last_eval_age_seconds: 1, monitors_by_status: {} };

function renderAt(path: string, replies: Record<string, Reply>) {
  const calls = fakeServer({ "GET /api/v1/monitors/status": { body: status }, ...replies });
  render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: [path] })} />);
  return calls;
}

afterEach(() => vi.unstubAllGlobals());

describe("MonitorsIndex", () => {
  it("lists worst first, and says why a monitor has no status", async () => {
    renderAt("/monitors", {
      "GET /api/v1/monitors": {
        body: {
          monitors: [
            monitor({ id: 1, name: "a fine one", status: "OK", groups: { OK: 2 } }),
            monitor({ id: 2, name: "b off", status: null, groups: {}, enabled: false }),
            monitor({ id: 3, name: "c broken", status: null, groups: {}, problem: "query: unknown aggregator" }),
            monitor({ id: 4, name: "d red", status: "ALERT", groups: { ALERT: 1, OK: 3 } }),
            monitor({ id: 5, name: "e new", status: null, groups: {} }),
          ],
        },
      },
    });
    await screen.findByRole("link", { name: "d red" });
    const names = screen.getAllByRole("row").slice(1).map((r) => within(r).getByRole("link").textContent);
    expect(names).toEqual(["d red", "a fine one", "b off", "c broken", "e new"]);
    const row = (name: string) => screen.getByRole("link", { name }).closest("tr")!;
    expect(row("b off")).toHaveTextContent("disabled");
    expect(row("c broken")).toHaveTextContent("not running");
    expect(row("c broken")).toHaveTextContent("query: unknown aggregator");
    expect(row("e new")).toHaveTextContent("no data yet");
    // None of the three is shown as OK.
    for (const n of ["b off", "c broken", "e new"]) expect(within(row(n)).queryByText("OK")).toBeNull();
    expect(within(row("d red")).getByLabelText("groups by status")).toHaveTextContent("1 ALERT3 OK");
  });

  it("asks the server with the filters in the URL", async () => {
    const calls = renderAt("/monitors?status=ALERT&tag=team:shop", { "GET /api/v1/monitors": { body: { monitors: [] } } });
    expect(await screen.findByText("No monitor matches these filters.")).toBeInTheDocument();
    const asked = calls.find((c) => c.path === "/api/v1/monitors")!;
    expect(Object.fromEntries(asked.search)).toEqual({ status: "ALERT", tag: "team:shop" });
  });

  it("mutes for an hour and unmutes", async () => {
    const user = userEvent.setup();
    let muted: number | null = null;
    const calls = renderAt("/monitors", {
      "GET /api/v1/monitors": () => ({ body: { monitors: [monitor({ muted_until: muted })] } }),
      "POST /api/v1/monitors/1/mute": (b) => {
        muted = (b as { until: number }).until;
        return { body: monitor({ muted_until: muted }) };
      },
      "POST /api/v1/monitors/1/unmute": () => {
        muted = null;
        return { body: monitor() };
      },
    });
    await user.click(await screen.findByRole("button", { name: "Mute 1h" }));
    const until = (calls.find((c) => c.path.endsWith("/mute"))!.body as { until: number }).until;
    expect(until - Date.now() / 1000).toBeGreaterThan(3500);
    await user.click(await screen.findByRole("button", { name: "Unmute" }));
    expect(await screen.findByRole("button", { name: "Mute 1h" })).toBeInTheDocument();
  });

  it("shows the server's sentence when a mute fails", async () => {
    const user = userEvent.setup();
    renderAt("/monitors", {
      "GET /api/v1/monitors": { body: { monitors: [monitor()] } },
      "POST /api/v1/monitors/1/mute": { status: 404, body: { error: "no monitor with id 1" } },
    });
    await user.click(await screen.findByRole("button", { name: "Mute 1h" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("no monitor with id 1"));
  });
});
