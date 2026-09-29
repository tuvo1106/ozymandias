import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createBrowserRouter, createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../../app/routes";
import { staleAfterSave } from "./DashboardEditor";
import { REVOKE_AFTER_MS } from "./JsonPanel";
import type { TimeseriesChartProps } from "../../../charts/TimeseriesChart";

// jsdom has no canvas; the chart stands in with what it was handed.
vi.mock("../../../charts/TimeseriesChart", () => ({
  TimeseriesChart: ({ labels }: TimeseriesChartProps) => <div data-testid="chart">{labels.join(" | ")}</div>,
}));
vi.mock("../../../charts/HeatmapChart", () => ({ HeatmapChart: () => <div data-testid="heatmap" /> }));

const stored = {
  id: 2,
  provisioned: false,
  created_at: "t",
  updated_at: "t",
  uid: "checkout",
  title: "Checkout",
  widgets: [
    {
      id: "req",
      type: "timeseries",
      title: "Throughput",
      layout: { x: 0, y: 0, w: 6, h: 3 },
      queries: [{ q: "sum:http.request.count{*}" }],
    },
    {
      id: "top",
      type: "toplist",
      title: "Top routes",
      layout: { x: 6, y: 0, w: 6, h: 3 },
      queries: [{ q: "sum:http.request.count{*} by {route}", reducer: "p42" }],
    },
  ],
};

const series = (label: string) => ({ metric: "http.request.count", tags: label ? { route: label } : {}, points: [[1, 1]] });

interface Reply {
  status?: number;
  body: unknown;
}
type Api = (path: string, init: RequestInit | undefined, url: URL) => Reply | "hang" | "unreachable";

const baseApi: Api = (path, init) => {
  if (path === "/api/v1/dashboards/2" && (!init?.method || init.method === "GET")) return { body: stored };
  if (path === "/api/v1/dashboards/3") return { body: { ...stored, id: 3, provisioned: true } };
  if (path === "/api/v1/query/batch") {
    const body = JSON.parse(String(init?.body)) as { queries: { q: string }[] };
    return {
      body: {
        from: 0,
        to: 60,
        results: body.queries.map((q, index) => ({
          index,
          status: "ok",
          query: q.q,
          interval: 10,
          series: [series("")],
          warnings: [],
        })),
      },
    };
  }
  if (path === "/api/v1/query/validate") return { body: { ok: true, query: "x" } };
  if (path === "/api/v1/dashboards/services") return { body: { services: [], truncated: false } };
  if (path === "/api/v1/dashboards") return { body: { dashboards: [], unreadable: [] } };
  if (path.startsWith("/api/v1/dashboards/service/")) return { body: { dashboards: [] } };
  if (path === "/api/v1/tags/values" || path === "/api/v1/tags" || path === "/api/v1/metrics") return { body: { values: [], keys: [], metrics: [] } };
  return { status: 404, body: { error: `no route for ${path}` } };
};

function mockApi(api: Api = baseApi) {
  const f = vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(input, "http://localhost");
    const reply = api(url.pathname, init, url) ?? baseApi(url.pathname, init, url);
    if (reply === "hang") return new Promise<Response>(() => {});
    if (reply === "unreachable") throw new TypeError("Failed to fetch");
    return new Response(JSON.stringify(reply.body), { status: reply.status ?? 200 });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

/** An api that overrides some routes and falls through to the base one. */
const over =
  (f: (path: string, init: RequestInit | undefined, url: URL) => Reply | "hang" | "unreachable" | undefined): Api =>
  (path, init, url) =>
    f(path, init, url) ?? baseApi(path, init, url);

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const sent = (f: ReturnType<typeof mockApi>, method: string) =>
  f.mock.calls
    .filter(([, init]) => (init as RequestInit | undefined)?.method === method)
    .map(([u, init]) => ({ url: String(u), body: JSON.parse(String((init as RequestInit).body)) as Record<string, unknown> }))
    .filter((c) => c.url.startsWith("/api/v1/dashboards"));

async function openEditor(path = "/dashboards/2/edit") {
  const router = renderAt(path);
  await screen.findByRole("heading", { level: 1 });
  return router;
}

const exported = async () => {
  await userEvent.click(screen.getByRole("button", { name: "JSON" }));
  return JSON.parse((screen.getByRole("textbox", { name: "Exported definition" }) as HTMLTextAreaElement).value);
};

afterEach(() => vi.unstubAllGlobals());

describe("saving", () => {
  it("PUTs the definition without the database's fields, and says it saved", async () => {
    const f = mockApi(over((path, init) => (path === "/api/v1/dashboards/2" && init?.method === "PUT" ? { body: { ...stored, title: "Checkout v2" } } : undefined)));
    await openEditor();
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.clear(title);
    await userEvent.type(title, "Checkout v2");
    expect(screen.getByText(/unsaved changes/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    const [put] = sent(f, "PUT");
    expect(put?.url).toBe("/api/v1/dashboards/2");
    expect(put?.body).not.toHaveProperty("id");
    expect(put?.body).not.toHaveProperty("provisioned");
    expect(put?.body).toMatchObject({ uid: "checkout", title: "Checkout v2" });
    expect(screen.queryByText(/unsaved changes/)).not.toBeInTheDocument();
  });

  it("lists every problem a refusal names", async () => {
    mockApi(
      over((_path, init) =>
        init?.method === "PUT"
          ? { status: 400, body: { error: "invalid dashboard: title is required\ninvalid dashboard: widgets[1] (top) query[0]: reducer \"p42\" is not one of last, avg, sum, min, max" } }
          : undefined,
      ),
    );
    await openEditor();
    await userEvent.clear(screen.getByRole("textbox", { name: "Title" }));
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    const alert = await screen.findByRole("alert");
    expect(within(alert).getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "title is required",
      'widgets[1] (top) query[0]: reducer "p42" is not one of last, avg, sum, min, max',
    ]);
  });

  it("shows a conflict in the server's words rather than guessing which conflict", async () => {
    mockApi(over((_p, init) => (init?.method === "PUT" ? { status: 409, body: { error: 'uid "checkout" is used by dashboard 5' } } : undefined)));
    await openEditor();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    expect(await screen.findByText('Not saved: uid "checkout" is used by dashboard 5')).toBeInTheDocument();
  });

  it("offers to save a deleted dashboard's draft as new", async () => {
    const f = mockApi(
      over((path, init) => {
        if (init?.method === "PUT") return { status: 404, body: { error: "no dashboard 2" } };
        if (path === "/api/v1/dashboards" && init?.method === "POST") return { status: 201, body: { ...stored, id: 9 } };
        if (path === "/api/v1/dashboards/9") return { body: { ...stored, id: 9 } };
        return undefined;
      }),
    );
    const router = await openEditor();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await userEvent.click(await screen.findByRole("button", { name: "Save it as a new dashboard" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/dashboards/9/edit"));
    expect(sent(f, "POST")).toHaveLength(1);
    // The router keeps the page mounted across /2/edit → /9/edit, so a flag
    // read once on mount would miss this arrival.
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.state).toBeNull());
  });

  it("does not claim a create that lost its answer wrote nothing", async () => {
    mockApi(over((path, init) => (path === "/api/v1/dashboards" && init?.method === "POST" ? "unreachable" : undefined)));
    renderAt("/dashboards/new");
    await userEvent.click(await screen.findByRole("button", { name: "Create" }));
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/Whether this was saved is not known/);
    expect(alert).toHaveTextContent(/check the dashboard list before creating it again/);
  });

  // A 2xx means the row exists. Saving again would create a second one.
  it("reports a create whose answer it cannot read as saved, and stops a second create", async () => {
    const f = mockApi(over((path, init) => (path === "/api/v1/dashboards" && init?.method === "POST" ? { status: 201, body: { weird: true } } : undefined)));
    renderAt("/dashboards/new");
    await userEvent.click(await screen.findByRole("button", { name: "Create" }));
    expect(await screen.findByText(/Saved — ozyd said so — but its answer could not be read/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Create" })).toBeDisabled();
    expect(sent(f, "POST")).toHaveLength(1);
  });
});

describe("where a draft comes from", () => {
  it("does not open a provisioned dashboard for editing, and offers a copy", async () => {
    mockApi();
    renderAt("/dashboards/3/edit");
    expect(await screen.findByText(/provisioned from a file/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "save a copy" })).toHaveAttribute("href", "/dashboards/new?copy=3");
    expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
  });

  it("copies a stored dashboard without its uid, and creates it", async () => {
    const f = mockApi(
      over((path, init) => {
        if (path === "/api/v1/dashboards" && init?.method === "POST") return { status: 201, body: { ...stored, id: 7 } };
        if (path === "/api/v1/dashboards/7") return { body: { ...stored, id: 7 } };
        return undefined;
      }),
    );
    const router = renderAt("/dashboards/new?copy=2");
    expect(await screen.findByRole("heading", { name: "Checkout (copy)" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Create" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/dashboards/7/edit"));
    expect(sent(f, "POST")[0]?.body).not.toHaveProperty("uid");
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
  });

  it("says a service copy names a template that is not there, rather than opening a blank page", async () => {
    mockApi();
    renderAt("/dashboards/new?service=api&template=99");
    expect(await screen.findByRole("alert")).toHaveTextContent("No template #99 covers api");
  });

  it("says a copy could not be loaded", async () => {
    mockApi(over((path) => (path === "/api/v1/dashboards/4" ? { status: 404, body: { error: "no dashboard 4" } } : undefined)));
    renderAt("/dashboards/new?copy=4");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load the dashboard to copy: no dashboard 4");
  });
});

describe("what the editor shows of a definition", () => {
  it("shows a reducer this build does not know as itself, not as the first option", async () => {
    mockApi();
    await openEditor();
    await userEvent.click(screen.getByRole("region", { name: "Top routes" }));
    const reducer = screen.getByRole("combobox", { name: "Reducer (over time)" }) as HTMLSelectElement;
    expect(reducer.selectedOptions[0]?.textContent).toBe('"p42" (not known to this build)');
    expect(screen.getByText(/This build does not know "p42"/)).toBeInTheDocument();
    // Untouched, it is saved as it was.
    expect((await exported()).widgets[1].queries[0].reducer).toBe("p42");
  });

  it("keeps what a type change leaves behind, lists it, and removes it on request", async () => {
    mockApi();
    await openEditor();
    await userEvent.click(screen.getByRole("region", { name: "Top routes" }));
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Type" }), "Timeseries");
    const leftover = screen.getByRole("group", { name: "Not used by this type" });
    expect(leftover).toHaveTextContent("queries[0].reducer");
    await userEvent.click(within(leftover).getByRole("button", { name: "Remove" }));
    expect(screen.queryByRole("group", { name: "Not used by this type" })).not.toBeInTheDocument();
    expect((await exported()).widgets[1].queries[0]).not.toHaveProperty("reducer");
  });

  it("does not choose a reducer for a new toplist", async () => {
    mockApi();
    await openEditor();
    await userEvent.selectOptions(screen.getByRole("combobox", { name: "Widget type to add" }), "Toplist");
    await userEvent.click(screen.getByRole("button", { name: "Add widget" }));
    expect(screen.getByText("Required, and not chosen for you.")).toBeInTheDocument();
  });

  it("never writes a half-typed number, and treats blank as absent", async () => {
    mockApi(over((path) => (path === "/api/v1/dashboards/2" ? { body: { ...stored, widgets: [{ ...stored.widgets[1], type: "query_value", reducer: undefined, queries: [{ q: "sum:x{*}", reducer: "avg" }], precision: 2 }] } } : undefined)));
    await openEditor();
    await userEvent.click(screen.getByRole("region", { name: "Top routes" }));
    const places = screen.getByRole("textbox", { name: "Decimal places" });
    await userEvent.clear(places);
    await userEvent.type(places, "1.5");
    // "1.5" passed through "1", which was written; the page says so rather
    // than implying the box and the definition agree.
    expect(screen.getByText("Must be a whole number. Until it is, the dashboard keeps 1.")).toBeInTheDocument();
    expect((await exported()).widgets[0].precision).toBe(1);
    await userEvent.click(screen.getByRole("region", { name: "Top routes" }));
    await userEvent.clear(screen.getByRole("textbox", { name: "Decimal places" }));
    expect((await exported()).widgets[0]).not.toHaveProperty("precision");
  });

  it("gives a widget type it does not know a panel that says so", async () => {
    mockApi(over((path) => (path === "/api/v1/dashboards/2" ? { body: { ...stored, widgets: [{ ...stored.widgets[0], type: "flamegraph", title: "Flames" }] } } : undefined)));
    await openEditor();
    await userEvent.click(screen.getByRole("region", { name: "Flames" }));
    expect(screen.getByText(/does not know what a "flamegraph" widget uses/)).toBeInTheDocument();
    expect((screen.getByRole("combobox", { name: "Type" }) as HTMLSelectElement).selectedOptions[0]?.textContent).toBe(
      '"flamegraph" (not known to this build)',
    );
  });
});

describe("the grid", () => {
  it("moves and resizes a widget from the keyboard, pushing what it lands on down", async () => {
    mockApi();
    await openEditor();
    const move = screen.getByRole("button", { name: "Move Throughput" });
    move.focus();
    await userEvent.keyboard("{ArrowRight}{ArrowRight}");
    await userEvent.click(screen.getByRole("button", { name: "Resize Throughput" }));
    await userEvent.keyboard("{ArrowDown}");
    const widgets = (await exported()).widgets;
    expect(widgets[0].layout).toEqual({ x: 2, y: 0, w: 6, h: 4 });
    expect(widgets[1].layout).toEqual({ x: 6, y: 4, w: 6, h: 3 });
  });
});

describe("the preview", () => {
  it("does not draw an old answer under an edited query", async () => {
    let calls = 0;
    mockApi(
      over((path) => {
        if (path !== "/api/v1/query/batch") return undefined;
        calls++;
        return calls === 1 ? undefined : "hang"; // the first answer lands; the next never does
      }),
    );
    await openEditor();
    const region = screen.getByRole("region", { name: "Throughput" });
    await waitFor(() => expect(within(region).getByTestId("chart")).toHaveTextContent("http.request.count{*}"));
    await userEvent.click(region);
    await userEvent.type(screen.getByRole("combobox", { name: "Query 1" }), " / 2");
    // Straight away: the preview has not asked yet, and says so.
    expect(within(screen.getByRole("region", { name: "Throughput" }).parentElement as HTMLElement).getByText("Updating preview…")).toBeInTheDocument();
    // Once it has asked (and the answer hangs), the old line is gone, not relabelled.
    await waitFor(() => expect(calls).toBeGreaterThan(1));
    await waitFor(() => expect(within(screen.getByRole("region", { name: "Throughput" })).getByTestId("chart")).toHaveTextContent(/^$/));
  });
});

describe("import and export", () => {
  it("replaces the draft with an import, leaving out the database's fields and saying so", async () => {
    mockApi();
    await openEditor();
    await userEvent.click(screen.getByRole("button", { name: "JSON" }));
    const box = screen.getByRole("textbox", { name: "Definition to import" });
    await userEvent.click(box);
    await userEvent.paste(JSON.stringify({ id: 4, provisioned: true, title: "Imported", widgets: [{ id: "n", type: "note", layout: { x: 0, y: 0, w: 4, h: 1 }, markdown: "hi" }] }));
    expect(screen.getByText(/"Imported", 1 widget\. Its id, provisioned will be left out/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Replace the draft" }));
    expect(screen.getByRole("heading", { name: "Imported" })).toBeInTheDocument();
    expect(screen.getByText(/Left out id, provisioned/)).toBeInTheDocument();
  });

  it("names what is wrong with an import and does not offer to use it", async () => {
    mockApi();
    await openEditor();
    await userEvent.click(screen.getByRole("button", { name: "JSON" }));
    await userEvent.click(screen.getByRole("textbox", { name: "Definition to import" }));
    await userEvent.paste('{"widgets": 3}');
    expect(screen.getByText("title is missing or not a string.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Replace the draft" })).toBeDisabled();
  });
});

describe("review fixes: the draft survives", () => {
  it("a change of view scope on a copy keeps the seed, and the draft", async () => {
    mockApi();
    const router = renderAt("/dashboards/new?copy=2");
    await screen.findByRole("heading", { name: "Checkout (copy)" });
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.type(title, " edited");
    await userEvent.click(screen.getByRole("checkbox", { name: /Auto-refresh/ }));
    expect(router.state.location.search).toContain("copy=2");
    expect(router.state.location.search).toContain("live=0");
    expect(screen.getByRole("heading", { name: "Checkout (copy) edited" })).toBeInTheDocument();
  });

  it("a failed refetch after a save keeps the editor on screen", async () => {
    let gets = 0;
    mockApi(
      over((path, init) => {
        if (path !== "/api/v1/dashboards/2") return undefined;
        if (init?.method === "PUT") return { body: stored };
        gets++;
        // A 4xx, which the app does not retry, so the query really does end
        // in error while holding its data — a 5xx would still be retrying
        // when the assertions run, and the test would pin nothing.
        return gets === 1 ? { body: stored } : { status: 403, body: { error: "gone away" } };
      }),
    );
    await openEditor();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(gets).toBeGreaterThan(1));
    // Give the failed refetch time to settle into the query's error state.
    await new Promise((r) => setTimeout(r, 50));
    expect(screen.getByRole("heading", { name: "Checkout!" })).toBeInTheDocument();
    expect(screen.queryByText("gone away")).not.toBeInTheDocument();
  });

  it("saving does not re-run the preview", async () => {
    let batches = 0;
    mockApi(
      over((path, init) => {
        if (path === "/api/v1/query/batch") batches++;
        if (path === "/api/v1/dashboards/2" && init?.method === "PUT") return { body: stored };
        return undefined;
      }),
    );
    await openEditor();
    await waitFor(() => expect(batches).toBe(1));
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await screen.findByText("Saved.");
    await new Promise((r) => setTimeout(r, 50));
    expect(batches).toBe(1);
  });

  it("clearing a field and typing it back is not an unsaved change", async () => {
    mockApi();
    await openEditor();
    await userEvent.click(screen.getByRole("region", { name: "Throughput" }));
    const title = screen.getByRole("textbox", { name: "Title" });
    await userEvent.clear(title);
    await userEvent.type(title, "Throughput");
    expect(screen.queryByText(/unsaved changes/)).not.toBeInTheDocument();
  });
});

describe("review fixes: what a create may do next", () => {
  it("after 'save as new' loses its answer, says to check rather than that retrying is safe", async () => {
    mockApi(
      over((path, init) => {
        if (init?.method === "PUT") return { status: 404, body: { error: "no dashboard 2" } };
        if (path === "/api/v1/dashboards" && init?.method === "POST") return "unreachable";
        return undefined;
      }),
    );
    await openEditor();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await userEvent.click(await screen.findByRole("button", { name: "Save it as a new dashboard" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/check the dashboard list before creating it again/);
  });

  it("after 'save as new' returns an unreadable answer, locks saving", async () => {
    mockApi(
      over((path, init) => {
        if (init?.method === "PUT") return { status: 404, body: { error: "no dashboard 2" } };
        if (path === "/api/v1/dashboards" && init?.method === "POST") return { status: 201, body: {} };
        return undefined;
      }),
    );
    await openEditor();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "!");
    await userEvent.click(screen.getByRole("button", { name: "Save" }));
    await userEvent.click(await screen.findByRole("button", { name: "Save it as a new dashboard" }));
    expect(await screen.findByText(/does not know the new dashboard/)).toBeInTheDocument();
    await userEvent.type(screen.getByRole("textbox", { name: "Title" }), "?");
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
  });

  it("does not carry a 'Saved.' banner in history past the render it was for", async () => {
    mockApi(
      over((path, init) => {
        if (path === "/api/v1/dashboards" && init?.method === "POST") return { status: 201, body: { ...stored, id: 7 } };
        if (path === "/api/v1/dashboards/7") return { body: { ...stored, id: 7 } };
        return undefined;
      }),
    );
    const router = renderAt("/dashboards/new");
    await userEvent.click(await screen.findByRole("button", { name: "Create" }));
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.state).toBeNull());
    expect(router.state.location.pathname).toBe("/dashboards/7/edit");
  });
});

describe("review fixes: download", () => {
  // Firefox ignores a click on a detached anchor, and revoking the URL in the
  // same task can pull the blob out from under the download.
  it("clicks an attached anchor and revokes the URL only afterwards", async () => {
    mockApi();
    const revoke = vi.fn();
    // jsdom has neither; set them on the real URL and put it back after.
    const saved = { create: URL.createObjectURL, revoke: URL.revokeObjectURL };
    URL.createObjectURL = () => "blob:x";
    URL.revokeObjectURL = revoke;
    let attached: boolean | undefined;
    const click = vi.spyOn(HTMLAnchorElement.prototype, "click").mockImplementation(function (this: HTMLAnchorElement) {
      attached = this.isConnected;
      expect(revoke).not.toHaveBeenCalled();
    });
    await openEditor();
    await userEvent.click(screen.getByRole("button", { name: "JSON" }));
    const timeout = vi.spyOn(window, "setTimeout");
    fireEvent.click(screen.getByRole("button", { name: "Download .json" }));
    expect(attached).toBe(true);
    expect(revoke).not.toHaveBeenCalled();
    // Not the next task: a download may not have started reading by then.
    const deferred = timeout.mock.calls.find(([, ms]) => ms === REVOKE_AFTER_MS);
    expect(deferred).toBeDefined();
    (deferred?.[0] as () => void)();
    expect(revoke).toHaveBeenCalledWith("blob:x");
    timeout.mockRestore();
    click.mockRestore();
    URL.createObjectURL = saved.create;
    URL.revokeObjectURL = saved.revoke;
  });
});

describe("review fixes: the 'Saved.' banner across a real reload", () => {
  // A memory router keeps state in memory; a reload keeps it in
  // window.history. This runs the browser router over jsdom's History API,
  // with a remount standing in for the reload.
  it("clears the flag from window.history, so a reload does not repeat the banner", async () => {
    mockApi();
    window.history.replaceState({ usr: { saved: true }, key: "k", idx: 0 }, "", "/dashboards/2/edit");
    const first = render(<RouterProvider router={createBrowserRouter(routes)} />);
    expect(await screen.findByText("Saved.")).toBeInTheDocument();
    await waitFor(() => expect((window.history.state as { usr?: unknown } | null)?.usr ?? null).toBeNull());
    first.unmount();
    render(<RouterProvider router={createBrowserRouter(routes)} />);
    await screen.findByRole("heading", { name: "Checkout" });
    expect(screen.queryByText("Saved.")).not.toBeInTheDocument();
  });
});

describe("review fixes: what a save invalidates", () => {
  it("refetches every definition, not the preview's data or another section's queries", () => {
    for (const k of [["dashboards", "list"], ["dashboards", "one", 2], ["dashboards", "services"], ["dashboards", "service", "checkout"]])
      expect(staleAfterSave(k)).toBe(true);
    // A definition query added later is covered without being listed.
    expect(staleAfterSave(["dashboards", "byUid", "checkout"])).toBe(true);
    for (const k of [["dashboards", "batch", 2, "q"], ["dashboards", "sketches", 2, "q"], ["monitors", "list"], ["logs", "service", "x"]])
      expect(staleAfterSave(k)).toBe(false);
  });
});
