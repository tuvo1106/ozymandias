import { render, screen, within } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { DashboardQuery, Widget } from "../../lib/dashboard";
import type { BatchResult } from "../../lib/dashboardsApi";
import { DashboardGrid } from "./DashboardGrid";
import { QueryValueWidget, TableWidget, TimeseriesWidget, ToplistWidget } from "./widgets";

// Both chart wrappers, because uPlot calls matchMedia when its module loads
// and jsdom has none. They are exercised in the browser; what these tests are
// about is which widget draws what.
vi.mock("../../charts/TimeseriesChart", () => ({
  TimeseriesChart: ({ labels }: { labels: string[] }) => <div data-testid="chart">{labels.join(" | ")}</div>,
}));

vi.mock("../../charts/HeatmapChart", () => ({
  HeatmapChart: () => <div data-testid="heatmap" />,
}));

const ok = (index: number, series: BatchResult["series"], query = "sum:x{*}"): BatchResult => ({
  index,
  status: "ok",
  query,
  interval: 60,
  series,
  warnings: [],
});

const failed = (index: number, error: string): BatchResult => ({
  index,
  status: "error",
  query: "bad",
  interval: 0,
  series: [],
  warnings: [],
  code: 400,
  error,
});

const line = (tags: Record<string, string>, values: number[]) => ({
  metric: "x",
  tags,
  points: values.map((v, i) => [1_790_000_000_000 + i * 60_000, v] as [number, number | null]),
});

const widget = (over: Partial<Widget> = {}): Widget => ({
  id: "w",
  type: "timeseries",
  title: "W",
  layout: { x: 0, y: 0, w: 6, h: 3 },
  queries: [{ q: "sum:x{*}" }],
  ...over,
});

describe("a widget that has not heard back", () => {
  // "No data" before anything has arrived tells the reader their service is
  // silent. It is the one thing a dashboard must not say by accident.
  it.each([
    ["timeseries", TimeseriesWidget],
    ["query_value", QueryValueWidget],
    ["toplist", ToplistWidget],
    ["table", TableWidget],
  ])("says nothing rather than No data (%s)", (type, Renderer) => {
    render(<Renderer widget={widget({ type: type as Widget["type"], queries: [{ q: "sum:x{*}", reducer: "last" }] })} results={[undefined]} />);
    expect(screen.queryByText("No data")).not.toBeInTheDocument();
  });

  it("says No data once an answer with no series arrives", () => {
    render(<TimeseriesWidget widget={widget()} results={[ok(0, [])]} />);
    expect(screen.getByText("No data")).toBeInTheDocument();
  });

  // Nothing was ever sent for a blank query, so no answer is coming and the
  // wait would never end.
  it("says No data for a widget whose only query is blank", () => {
    render(<TimeseriesWidget widget={widget({ queries: [{ q: "  " }] })} results={[undefined]} />);
    expect(screen.getByText("No data")).toBeInTheDocument();
  });
});

describe("a widget with one refused query", () => {
  it("still draws the query that answered", () => {
    const w = widget({ queries: [{ q: "sum:x{*}" }, { q: "bad" }] });
    render(<TimeseriesWidget widget={w} results={[ok(0, [line({}, [1, 2])]), failed(1, "unknown aggregator")]} />);
    expect(screen.getByTestId("chart")).toBeInTheDocument();
    expect(screen.getByText("unknown aggregator")).toBeInTheDocument();
  });

  it("does not also say No data, which would read as a second problem", () => {
    render(<TimeseriesWidget widget={widget({ queries: [{ q: "bad" }] })} results={[failed(0, "unknown aggregator")]} />);
    expect(screen.getByText("unknown aggregator")).toBeInTheDocument();
    expect(screen.queryByText("No data")).not.toBeInTheDocument();
  });
});

describe("reducers", () => {
  // Each query carries its own rule. One taken from queries[0] and applied to
  // every line ranks the rows by two different questions.
  it("reduces each toplist row by its own query's reducer", () => {
    const w = widget({
      type: "toplist",
      queries: [
        { q: "sum:a{*} by {r}", reducer: "max" },
        { q: "sum:b{*} by {r}", reducer: "sum" },
      ],
    });
    const { container } = render(<ToplistWidget widget={w} results={[ok(0, [line({ r: "a" }, [1, 9])]), ok(1, [line({ r: "b" }, [1, 9])])]} />);
    const rows = [...container.querySelectorAll("ol > li")].map((li) => li.textContent);
    // max of [1,9] is 9; sum of [1,9] is 10 — not 9 and 9, and not 10 and 10.
    expect(rows).toEqual(["x{r:b}10", "x{r:a}9"]);
  });

  // The first line belongs to the second query whenever the first errored.
  it("reduces a query_value by the reducer of the query the line came from", () => {
    const w = widget({
      type: "query_value",
      queries: [
        { q: "bad", reducer: "last" },
        { q: "sum:b{*}", reducer: "sum" },
      ],
    });
    render(<QueryValueWidget widget={w} results={[failed(0, "nope"), ok(1, [line({}, [1, 2, 3])])]} />);
    expect(screen.getByText("6")).toBeInTheDocument();
  });

  // React renders both rows either way and complains to the console instead,
  // so the duplicate key is only visible there — and what it costs is
  // reconciliation reusing the wrong row on the next refresh, which a render
  // of a static tree cannot show.
  it("gives two rows for the same group two different keys", () => {
    const complain = vi.spyOn(console, "error").mockImplementation(() => {});
    const w = widget({
      type: "toplist",
      queries: [
        { q: "sum:a{*} by {r}", reducer: "last" },
        { q: "sum:b{*} by {r}", reducer: "last" },
      ],
    });
    const { container } = render(<ToplistWidget widget={w} results={[ok(0, [line({ r: "same" }, [1])]), ok(1, [line({ r: "same" }, [2])])]} />);
    expect(container.querySelectorAll("ol > li")).toHaveLength(2);
    expect(complain.mock.calls.flat().join(" ")).not.toMatch(/same key/i);
    complain.mockRestore();
  });
});

describe("a reducer this build does not know", () => {
  // Same route as the unknown widget type below, one level down: a definition
  // is served back from the store without being re-validated, so `reducer` is
  // whatever is in the row.
  it("draws a dash rather than taking the page down", () => {
    const w = widget({
      type: "query_value",
      queries: [{ q: "sum:x{*}", reducer: "median" as DashboardQuery["reducer"] }],
      precision: 2,
    });
    render(<QueryValueWidget widget={w} results={[ok(0, [line({}, [1, 2, 3])])]} />);
    expect(screen.getByText("—")).toBeInTheDocument();
    expect(screen.getByText(/cannot apply a "median" reducer/)).toBeInTheDocument();
  });

  // Dropping the row silently is the failure to avoid: null already means
  // "this line had nothing measurable", so a reader would see a short ranking
  // and believe that was all the data. The row still goes; the widget says so.
  // "No data" means the service is not reporting (docs/ui.md §3). A toplist
  // whose every reducer is unknown has no rows *and* has series, so saying it
  // would tell the reader a reporting service is silent.
  it("does not also say No data when nothing could be reduced", () => {
    const w = widget({
      type: "toplist",
      queries: [{ q: "sum:a{*} by {r}", reducer: "median" as DashboardQuery["reducer"] }],
    });
    render(<ToplistWidget widget={w} results={[ok(0, [line({ r: "a" }, [5])])]} />);
    expect(screen.getByText(/cannot apply a "median" reducer/)).toBeInTheDocument();
    expect(screen.queryByText("No data")).not.toBeInTheDocument();
  });

  it("does not claim a table's query was left out when its column is still there", () => {
    const w = widget({
      type: "table",
      queries: [
        { q: "sum:a{*} by {r}", reducer: "median" as DashboardQuery["reducer"] },
        { q: "sum:b{*} by {r}", reducer: "sum" },
      ],
    });
    render(<TableWidget widget={w} results={[ok(0, [line({ r: "a" }, [5])]), ok(1, [line({ r: "a" }, [1, 2])])]} />);
    // The column is still drawn, with a dash in it — so the message must not
    // say the query was "left out".
    expect(screen.getAllByRole("columnheader")).toHaveLength(3);
    expect(screen.getByText(/nothing is shown for that query/)).toBeInTheDocument();
  });

  // The server accepts conditional_formats on a table; until the editor
  // mirrored its rules, the table drew none of them.
  it("colours a table's cells by its conditional formats, first match wins", () => {
    const w = widget({
      type: "table",
      queries: [{ q: "sum:a{*} by {r}", reducer: "last" }],
      conditional_formats: [
        { op: ">", value: 10, color: "red" },
        { op: ">", value: 1, color: "yellow" },
        { op: ">", value: 0, color: "chartreuse" },
      ],
    });
    render(
      <TableWidget
        widget={w}
        results={[ok(0, [line({ r: "hot" }, [50]), line({ r: "warm" }, [5]), line({ r: "odd" }, [0.5]), line({ r: "cold" }, [0])])]}
      />,
    );
    const cell = (group: string) => within(screen.getByRole("row", { name: new RegExp(group) })).getByRole("cell");
    expect(cell("hot").className).toMatch(/text-red-600/);
    expect(cell("warm").className).toMatch(/text-amber-600/);
    // A colour this build does not have draws the value plainly, not not at all.
    expect(cell("odd")).toHaveTextContent("0.5");
    expect(cell("odd").className).not.toMatch(/text-(red|amber|emerald|sky)/);
    expect(cell("cold").className).not.toMatch(/text-(red|amber)/);
  });

  it("says so rather than quietly shortening a toplist", () => {
    const w = widget({
      type: "toplist",
      queries: [
        { q: "sum:a{*} by {r}", reducer: "median" as DashboardQuery["reducer"] },
        { q: "sum:b{*} by {r}", reducer: "sum" },
      ],
    });
    const { container } = render(<ToplistWidget widget={w} results={[ok(0, [line({ r: "a" }, [5])]), ok(1, [line({ r: "b" }, [1, 2])])]} />);
    expect([...container.querySelectorAll("ol > li")].map((li) => li.textContent)).toEqual(["x{r:b}3"]);
    expect(screen.getByText(/cannot apply a "median" reducer/)).toBeInTheDocument();
  });
});

describe("a widget type this build does not know", () => {
  // The client validates `type` as "a string" because the server validates
  // definitions. A server newer than the bundle therefore produces one with no
  // renderer, and `<undefined />` throws through React Router's default
  // boundary, taking the whole application with it.
  it("says so in its own frame and leaves its neighbours alone", () => {
    const widgets = [
      widget({ id: "known", title: "Known" }),
      { ...widget({ id: "future", title: "Future" }), type: "flamegraph" as Widget["type"] },
    ];
    render(
      <DashboardGrid
        widgets={widgets}
        byWidget={new Map([["known", new Map([[0, { asked: widgets[0]!.queries![0]!.q.trim(), result: ok(0, [line({}, [1, 2])]) }]])]])}
        sketches={new Map()}
        syncKey="k"
      />,
    );
    expect(within(screen.getByRole("region", { name: "Future" })).getByText(/cannot draw a "flamegraph" widget/)).toBeInTheDocument();
    expect(within(screen.getByRole("region", { name: "Known" })).getByTestId("chart")).toBeInTheDocument();
  });
});
