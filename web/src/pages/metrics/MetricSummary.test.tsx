import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { routes } from "../../app/routes";

type Reply = { status?: number; body: unknown } | Promise<{ status?: number; body: unknown }>;
type Api = (url: URL) => Reply | undefined;

const list = (prefix: string) => {
  const all = [
    { name: "http.request.count", type: "count", series: 120 },
    { name: "queue.depth", type: null, series: 3 },
    { name: "http.request.duration.count", type: "count", series: 40 },
  ].filter((m) => m.name.startsWith(prefix));
  all.sort((a, b) => b.series - a.series);
  return { metrics: all, total: all.length, truncated: false };
};

const tags = (metric: string) =>
  metric === "http.request.count"
    ? {
        metric,
        type: "count",
        series: 120,
        keys: [
          { key: "route", series: 120, values: 40 },
          { key: "env", series: 100, values: 2 },
        ],
      }
    : metric === "bare.metric"
      ? { metric, type: "gauge", series: 1, keys: [] }
      : { metric, type: null, series: 0, keys: [] };

const base: Api = (url) => {
  if (url.pathname === "/api/v1/metrics/cardinality") return { body: list(url.searchParams.get("prefix") ?? "") };
  if (url.pathname === "/api/v1/tags/cardinality") return { body: tags(url.searchParams.get("metric") ?? "") };
  return { status: 404, body: { error: "no route" } };
};

function mockApi(api: Api = base) {
  const f = vi.fn(async (input: string) => {
    const url = new URL(input, "http://localhost");
    const { status = 200, body } = await (api(url) ?? base(url))!;
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}

const params = (router: ReturnType<typeof renderAt>) => new URLSearchParams(router.state.location.search);
const rows = (name: string) =>
  within(screen.getByRole("table", { name })).getAllByRole("row").slice(1).map((r) => r.textContent);

afterEach(() => vi.unstubAllGlobals());

describe("MetricSummary", () => {
  it("lists metrics highest first, with a type or says it was not recorded", async () => {
    mockApi();
    renderAt("/metrics/summary");
    await screen.findByRole("table", { name: "Series per metric" });
    expect(rows("Series per metric")).toEqual([
      "http.request.countcount120",
      "http.request.duration.countcount40",
      "queue.depthnot recorded3",
    ]);
    expect(screen.getByText("Choose a metric to see which tag keys make its series.")).toBeInTheDocument();
  });

  it("is a tab of the Metrics section, beside the Explorer", async () => {
    mockApi();
    renderAt("/metrics/summary");
    const tabs = await screen.findByRole("navigation", { name: "Metrics pages" });
    expect(within(tabs).getByRole("link", { name: "Explorer" })).toHaveAttribute("href", "/metrics/explorer");
    expect(within(tabs).getByRole("link", { name: "Summary" })).toHaveAttribute("aria-current", "page");
  });

  it("shows a metric's tag keys, most values first, and links to chart it", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/summary");
    await user.click(await screen.findByRole("button", { name: "http.request.count" }));
    await waitFor(() => expect(params(router).get("metric")).toBe("http.request.count"));
    await screen.findByRole("table", { name: "Series per tag key" });
    expect(rows("Series per tag key")).toEqual(["route40all", "env2100 of 120"]);
    expect(screen.getByText("120 series · count")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Chart it" })).toHaveAttribute(
      "href",
      `/metrics/explorer?${new URLSearchParams({ q: "avg:http.request.count{*}" })}`,
    );
  });

  // "No keys" is two answers; the series count says which.
  it("tells a metric with no tags from one the store does not have", async () => {
    mockApi();
    const router = renderAt("/metrics/summary?metric=bare.metric");
    expect(await screen.findByText(/Its series carry no tags/)).toBeInTheDocument();
    await router.navigate("/metrics/summary?metric=ghost");
    expect(await screen.findByText("This ozyd holds no series of ghost.")).toBeInTheDocument();
    expect(screen.queryByText(/carry no tags/)).not.toBeInTheDocument();
  });

  it("filters by prefix once typing pauses, writing it to the URL", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/summary");
    await screen.findByRole("table", { name: "Series per metric" });
    await user.type(screen.getByRole("searchbox"), "queue");
    await waitFor(() => expect(params(router).get("prefix")).toBe("queue"));
    await waitFor(() => expect(rows("Series per metric")).toEqual(["queue.depthnot recorded3"]));
    const prefixes = f.mock.calls
      .map(([u]) => new URL(u, "http://localhost"))
      .filter((u) => u.pathname === "/api/v1/metrics/cardinality")
      .map((u) => u.searchParams.get("prefix"));
    expect(prefixes).toEqual(["", "queue"]);
  });

  // The URL is trimmed; the box is the author's and keeps what they typed.
  it("does not strip a space from the box while the author types", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/metrics/summary");
    await screen.findByRole("table", { name: "Series per metric" });
    await user.type(screen.getByRole("searchbox"), "queue ");
    await waitFor(() => expect(params(router).get("prefix")).toBe("queue"));
    await waitFor(() => expect(rows("Series per metric")).toEqual(["queue.depthnot recorded3"]));
    expect(screen.getByRole("searchbox")).toHaveValue("queue ");
  });

  // Back must not be undone by the box's debounced value writing itself back.
  it("follows the URL when it changes under the box", async () => {
    mockApi();
    const router = renderAt("/metrics/summary?prefix=queue");
    await waitFor(() => expect(rows("Series per metric")).toEqual(["queue.depthnot recorded3"]));
    await router.navigate("/metrics/summary?prefix=http");
    await waitFor(() => expect(screen.getByRole("searchbox")).toHaveValue("http"));
    await new Promise((r) => setTimeout(r, 400));
    expect(params(router).get("prefix")).toBe("http");
  });

  it("names the previous prefix's list while the next one is counted", async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => (release = r));
    mockApi((u) =>
      u.pathname === "/api/v1/metrics/cardinality" && u.searchParams.get("prefix") === "http"
        ? gate.then(() => ({ body: list("http") }))
        : undefined,
    );
    const router = renderAt("/metrics/summary");
    await screen.findByRole("table", { name: "Series per metric" });
    await router.navigate("/metrics/summary?prefix=http");
    expect(await screen.findByText(/Counting for “http”… the list below is for every metric/)).toBeInTheDocument();
    release();
    await waitFor(() => expect(rows("Series per metric")).toHaveLength(2));
    expect(screen.queryByText(/Counting for/)).not.toBeInTheDocument();
  });

  // The previous prefix's list under a failed prefix would read as its answer.
  it("shows the failure, not the previous list, when a new prefix fails", async () => {
    mockApi((u) =>
      u.pathname === "/api/v1/metrics/cardinality" && u.searchParams.get("prefix") === "bad"
        ? { status: 500, body: { error: "the series counts could not be read" } }
        : undefined,
    );
    const router = renderAt("/metrics/summary");
    await screen.findByRole("table", { name: "Series per metric" });
    await router.navigate("/metrics/summary?prefix=bad");
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not count the series");
    expect(screen.queryByRole("table", { name: "Series per metric" })).not.toBeInTheDocument();
  });

  it("tells an empty store from an empty prefix", async () => {
    mockApi((u) => (u.pathname === "/api/v1/metrics/cardinality" && !u.searchParams.get("prefix") ? { body: { metrics: [], total: 0, truncated: false } } : undefined));
    const router = renderAt("/metrics/summary");
    expect(await screen.findByText("This ozyd holds no metrics yet.")).toBeInTheDocument();
    await router.navigate("/metrics/summary?prefix=zzz");
    expect(await screen.findByText("No metric starts with “zzz”.")).toBeInTheDocument();
  });

  it("says how much a truncated list left out", async () => {
    mockApi((u) =>
      u.pathname === "/api/v1/metrics/cardinality"
        ? { body: { metrics: [{ name: "a", type: "gauge", series: 9 }], total: 350, truncated: true } }
        : undefined,
    );
    renderAt("/metrics/summary");
    expect(await screen.findByText("The 1 highest of 350. Narrow the prefix to see the rest.")).toBeInTheDocument();
  });

  it("says when the counts could not be read", async () => {
    mockApi((u) =>
      u.pathname === "/api/v1/metrics/cardinality"
        ? { status: 500, body: { error: "the series counts could not be read" } }
        : u.pathname === "/api/v1/tags/cardinality"
          ? { status: 500, body: { error: "the series counts could not be read" } }
          : undefined,
    );
    renderAt("/metrics/summary?metric=m");
    await waitFor(() =>
      expect(screen.getAllByRole("alert").map((a) => a.textContent)).toEqual(
        expect.arrayContaining([
          "Could not count the series: the series counts could not be read",
          "Could not count its tag keys: the series counts could not be read",
        ]),
      ),
    );
  });
});
