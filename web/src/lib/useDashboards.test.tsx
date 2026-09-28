import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { DashboardRequests, QuerySlot } from "./dashboardQueries";
import { DEFAULT_VIEW_STATE } from "./dashboardState";
import { createQueryClient } from "./queryClient";
import { useDashboardData, useDashboardSketches } from "./useDashboards";

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const slot = (widgetId: string, q: string): QuerySlot => ({ widgetId, queryIndex: 0, q });

const requests = (...slots: QuerySlot[]): DashboardRequests => ({ chunks: [slots], heatmaps: [] });

/** One result per query, each carrying the widget's own number. */
function answer(values: number[]) {
  return new Response(
    JSON.stringify({
      status: "ok",
      from: 1_790_000_000,
      to: 1_790_003_600,
      results: values.map((v, index) => ({
        index,
        status: "ok",
        query: "q",
        interval: 60,
        series: [{ metric: "m", tags: {}, points: [[1_790_000_000_000, v]] }],
        warnings: [],
      })),
    }),
  );
}

const valueFor = (data: ReturnType<typeof useDashboardData>, widgetId: string) =>
  data.byWidget.get(widgetId)?.get(0)?.series[0]?.points[0]?.[1];

afterEach(() => vi.unstubAllGlobals());

describe("useDashboardData", () => {
  it("hands each widget the result whose index asked for it", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => answer([11, 22])));
    const { result } = renderHook(() => useDashboardData(requests(slot("a", "qa"), slot("b", "qb")), DEFAULT_VIEW_STATE, [], "d1"), {
      wrapper,
    });
    await waitFor(() => expect(result.current.byWidget.size).toBe(2));
    expect(valueFor(result.current, "a")).toBe(11);
    expect(valueFor(result.current, "b")).toBe(22);
  });

  // `keepPreviousData` serves the previous key's results while a new key is in
  // flight. Pairing those against the chunk the *current* render holds gives
  // widget b the number that belonged to widget a — one widget showing
  // another's, which is the worst failure a dashboard has. The slots travel
  // with the answer so that cannot happen.
  it("never attributes a kept answer to a widget that moved into its slot", async () => {
    const fetchMock = vi.fn(async () => answer([11, 22]));
    vi.stubGlobal("fetch", fetchMock);
    const { result, rerender } = renderHook(({ r }: { r: DashboardRequests }) => useDashboardData(r, DEFAULT_VIEW_STATE, [], "d1"), {
      wrapper,
      initialProps: { r: requests(slot("a", "qa"), slot("b", "qb")) },
    });
    await waitFor(() => expect(valueFor(result.current, "b")).toBe(22));

    // Widget a is deleted, so b is now slot 0 — and the next answer never
    // arrives, which is exactly when the kept one is on screen.
    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    rerender({ r: requests(slot("b", "qb")) });

    // Widget b keeps its *own* number, never widget a's.
    expect(valueFor(result.current, "b")).toBe(22);
  });

  // `placeholderData: keepPreviousData` does nothing for useQueries (5.103.1),
  // so without the hook's own keeping, changing the range blanks every widget
  // on the page until the new answer lands.
  it("keeps the last answer on screen while a new window is in flight", async () => {
    const fetchMock = vi.fn(async () => answer([11]));
    vi.stubGlobal("fetch", fetchMock);
    const r = requests(slot("a", "qa"));
    const { result, rerender } = renderHook(({ state }: { state: typeof DEFAULT_VIEW_STATE }) => useDashboardData(r, state, [], "d1"), {
      wrapper,
      initialProps: { state: DEFAULT_VIEW_STATE },
    });
    await waitFor(() => expect(valueFor(result.current, "a")).toBe(11));

    fetchMock.mockImplementation(() => new Promise<Response>(() => {}));
    rerender({ state: { ...DEFAULT_VIEW_STATE, range: { kind: "relative", preset: "4h" } } });
    expect(valueFor(result.current, "a")).toBe(11);
  });

  // A dashboard past 50 queries is several requests. One of them failing is
  // not the others' problem, for the same reason one query failing is not the
  // other queries' (ADR-0017).
  it("draws the chunks that answered when one of several fails", async () => {
    let call = 0;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        call += 1;
        return call === 1 ? answer([11]) : new Response(JSON.stringify({ error: "a batch takes at most 50 queries" }), { status: 400 });
      }),
    );
    const two: DashboardRequests = { chunks: [[slot("a", "qa")], [slot("b", "qb")]], heatmaps: [] };
    const { result } = renderHook(() => useDashboardData(two, DEFAULT_VIEW_STATE, [], "d1"), { wrapper });
    await waitFor(() => expect(valueFor(result.current, "a")).toBe(11));
    expect(result.current.error?.message).toBe("a batch takes at most 50 queries");
    expect(result.current.byWidget.has("b")).toBe(false);
  });

  // The cached value is keyed by widget id, so two dashboards holding the same
  // queries in the same order under different ids must not share an entry —
  // a saved copy of another dashboard, which this build invites by making a
  // template instance storable. Sharing files the answer under the first
  // one's ids and every widget on the second draws nothing at all, with
  // nothing to correct it: fresh for five seconds, and no interval on an
  // absolute range.
  it("does not hand one dashboard's answer to another with the same queries", async () => {
    // The app's own client, not a bare one: its `staleTime` is what makes a
    // shared entry stick instead of being refetched on the second mount, so a
    // test without it would pass whether the key is right or wrong.
    const client = createQueryClient();
    const shared = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    vi.stubGlobal("fetch", vi.fn(async () => answer([11])));

    const a = renderHook(() => useDashboardData(requests(slot("a-widget", "qa")), DEFAULT_VIEW_STATE, [], "dashboard-1"), { wrapper: shared });
    await waitFor(() => expect(valueFor(a.result.current, "a-widget")).toBe(11));

    const b = renderHook(() => useDashboardData(requests(slot("b-widget", "qa")), DEFAULT_VIEW_STATE, [], "dashboard-2"), { wrapper: shared });
    await waitFor(() => expect(valueFor(b.result.current, "b-widget")).toBe(11));
  });

  it("does not hand one dashboard's sketch to another with the same query", async () => {
    const client = createQueryClient();
    const shared = ({ children }: { children: ReactNode }) => <QueryClientProvider client={client}>{children}</QueryClientProvider>;
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(JSON.stringify({ from: 1, to: 2, interval: 60, bins: 0, series: [], warnings: [] }))),
    );
    const heatmap = (widgetId: string) => [{ widgetId, q: "dist:lat{*}" }];

    const a = renderHook(() => useDashboardSketches(heatmap("a-widget"), DEFAULT_VIEW_STATE, [], "dashboard-1"), { wrapper: shared });
    await waitFor(() => expect(a.result.current.get("a-widget")?.data).toBeDefined());

    const b = renderHook(() => useDashboardSketches(heatmap("b-widget"), DEFAULT_VIEW_STATE, [], "dashboard-2"), { wrapper: shared });
    await waitFor(() => expect(b.result.current.get("b-widget")?.data).toBeDefined());
  });

  it("reports a failed request rather than pretending the widgets are empty", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "a batch takes at most 50 queries" }), { status: 400 })));
    const { result } = renderHook(() => useDashboardData(requests(slot("a", "qa")), DEFAULT_VIEW_STATE, [], "d1"), { wrapper });
    await waitFor(() => expect(result.current.error?.message).toBe("a batch takes at most 50 queries"));
    expect(result.current.byWidget.size).toBe(0);
  });

  it("takes the x-axis window from the server's answer, not from the request", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => answer([1])));
    const { result } = renderHook(() => useDashboardData(requests(slot("a", "qa")), DEFAULT_VIEW_STATE, [], "d1"), { wrapper });
    await waitFor(() => expect(result.current.range).toEqual({ from: 1_790_000_000, to: 1_790_003_600 }));
  });
});
