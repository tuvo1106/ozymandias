import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { routes } from "../../app/routes";

const mk = (i: number, over: Record<string, unknown> = {}) => ({
  ts: 1_790_000_000_000 - i * 1000,
  message: `request ${i} handled`,
  status: i % 5 === 0 ? "error" : "info",
  service: "web-api",
  host: "box",
  attrs: { route: "/orders", user: { id: `u${i}` }, ms: i },
  ...over,
});

const logsBody = (n = 3, extra: Record<string, unknown> = {}) => ({
  logs: Array.from({ length: n }, (_, i) => mk(i + 1)),
  truncated: false,
  stats: { streams: 1, blocks_read: 2, blocks_skipped: 7, bytes_read: 1, entries_examined: 1 },
  ...extra,
});

type Handler = (url: URL) => { status?: number; body: unknown } | undefined;

function mockApi(h: Handler = () => undefined) {
  const f = vi.fn(async (input: string) => {
    const url = new URL(input, "http://localhost");
    const hit = h(url);
    if (hit) return new Response(JSON.stringify(hit.body), { status: hit.status ?? 200 });
    switch (url.pathname) {
      case "/api/v1/logs":
        return new Response(JSON.stringify(logsBody()));
      case "/api/v1/logs/aggregate":
        return new Response(JSON.stringify({ interval_ms: 1000, buckets: [{ ts: 1_789_999_990_000, counts: { info: 2, error: 1 } }], truncated: false }));
      case "/api/v1/logs/facets":
        return new Response(
          JSON.stringify({ facets: { status: [{ value: "info", count: 2 }, { value: "error", count: 1 }], service: [{ value: "web-api", count: 3 }] }, capped: [], truncated: false }),
        );
      default:
        return new Response("{}", { status: 404 });
    }
  });
  vi.stubGlobal("fetch", f);
  return f;
}

class FakeEventSource {
  static last: FakeEventSource | undefined;
  static all: FakeEventSource[] = [];
  listeners = new Map<string, (e: MessageEvent) => void>();
  onopen: ((e: Event) => void) | null = null;
  onerror: ((e: Event) => void) | null = null;
  closed = false;
  constructor(public url: string) {
    FakeEventSource.last = this;
    FakeEventSource.all.push(this);
  }
  addEventListener(t: string, fn: (e: MessageEvent) => void) {
    this.listeners.set(t, fn);
  }
  close() {
    this.closed = true;
  }
  emit(t: string, data: unknown) {
    this.listeners.get(t)?.({ data: JSON.stringify(data) } as MessageEvent);
  }
}

function renderAt(path: string) {
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  render(<RouterProvider router={router} />);
  return router;
}
const params = (r: ReturnType<typeof renderAt>) => new URLSearchParams(r.state.location.search);
const calls = (f: ReturnType<typeof mockApi>, path: string) =>
  f.mock.calls.map(([u]) => new URL(u, "http://localhost")).filter((u) => u.pathname === path);

beforeEach(() => {
  FakeEventSource.last = undefined;
  FakeEventSource.all = [];
  vi.stubGlobal("EventSource", FakeEventSource);
});
afterEach(() => vi.unstubAllGlobals());

describe("LogExplorer", () => {
  it("is live in the nav and searches the last 15 minutes on open, in milliseconds", async () => {
    const f = mockApi();
    renderAt("/logs");
    expect(await screen.findByRole("heading", { name: "Logs" })).toBeInTheDocument();
    const list = await screen.findByRole("list", { name: "Logs" });
    expect(within(list).getAllByRole("listitem")).toHaveLength(3);
    const u = calls(f, "/api/v1/logs")[0]!;
    const from = Number(u.searchParams.get("from"));
    const to = Number(u.searchParams.get("to"));
    expect(to - from).toBeGreaterThan(14 * 60_000);
    expect(to - from).toBeLessThan(16 * 60_000);
    expect(to).toBeGreaterThan(1_700_000_000_000); // ms, not seconds
    expect(screen.getByText(/2 blocks read, 7 skipped/)).toBeInTheDocument();
  });

  it("runs the typed query into the URL and asks all three endpoints", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    const router = renderAt("/logs");
    await user.type(await screen.findByRole("combobox", { name: "Log query" }), "status:error{Enter}");
    await waitFor(() => expect(params(router).get("q")).toBe("status:error"));
    await waitFor(() => {
      for (const p of ["/api/v1/logs", "/api/v1/logs/aggregate", "/api/v1/logs/facets"]) {
        expect(calls(f, p).some((u) => u.searchParams.get("q") === "status:error")).toBe(true);
      }
    });
  });

  it("builds a query from a facet click, and excludes on shift-click", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/logs?q=timeout");
    const status = await screen.findByRole("region", { name: "status facet" });
    await user.click(within(status).getByRole("button", { name: /error/ }));
    await waitFor(() => expect(params(router).get("q")).toBe("timeout status:error"));
    await user.keyboard("{Shift>}");
    await user.click(within(await screen.findByRole("region", { name: "service facet" })).getByRole("button", { name: /web-api/ }));
    await user.keyboard("{/Shift}");
    await waitFor(() => expect(params(router).get("q")).toBe("timeout status:error -service:web-api"));
  });

  it("shows the server's message for a query that does not parse", async () => {
    mockApi((u) => (u.pathname === "/api/v1/logs" ? { status: 400, body: { error: "col 9: unknown key" } } : undefined));
    renderAt("/logs?q=nope%3Ax");
    expect(await screen.findByRole("alert")).toHaveTextContent("col 9: unknown key");
  });

  it("opens a detail panel with the full message and attribute actions, and toggles a column", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/logs");
    const list = await screen.findByRole("list", { name: "Logs" });
    await user.click(within(list).getAllByRole("listitem")[0]!);
    const detail = await screen.findByRole("complementary", { name: "Log detail" });
    expect(within(detail).getByText("request 1 handled")).toBeInTheDocument();
    await user.click(within(detail).getByRole("button", { name: "Column route" }));
    await waitFor(() => expect(params(router).get("cols")).toBe("route"));
    expect(within(await screen.findByRole("list", { name: "Logs" })).getAllByText("/orders").length).toBeGreaterThan(0);
    await user.click(within(detail).getByRole("button", { name: "Filter user.id:u1" }));
    await waitFor(() => expect(params(router).get("q")).toBe("@user.id:u1"));
    await user.click(within(detail).getByRole("button", { name: "Close detail" }));
    expect(screen.queryByRole("complementary", { name: "Log detail" })).not.toBeInTheDocument();
  });

  it("narrows the range to a histogram bar", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/logs?tail=1");
    await user.click(await screen.findByRole("button", { name: /3 logs$/ }));
    await waitFor(() => expect(params(router).get("from")).toBe("1789999990"));
    expect(params(router).get("tail")).toBeNull();
  });

  it("pages on scroll with the previous cursor", async () => {
    const f = mockApi((u) => {
      if (u.pathname !== "/api/v1/logs") return undefined;
      return u.searchParams.get("cursor") === "c1"
        ? { body: { logs: [mk(900)], truncated: false } }
        : { body: logsBody(30, { cursor: "c1" }) };
    });
    renderAt("/logs");
    await screen.findByRole("list", { name: "Logs" });
    await waitFor(() => expect(calls(f, "/api/v1/logs").some((u) => u.searchParams.get("cursor") === "c1")).toBe(true));
  });

  it("tails: streams new logs above the list, pauses without losing them, reports drops, and closes", async () => {
    mockApi();
    const user = userEvent.setup();
    const router = renderAt("/logs?q=status%3Aerror");
    await screen.findByRole("list", { name: "Logs" });
    await user.click(screen.getByRole("button", { name: /Live tail/ }));
    await waitFor(() => expect(params(router).get("tail")).toBe("1"));
    const es = FakeEventSource.last!;
    expect(es.url).toBe("/api/v1/logs/tail?q=status%3Aerror");
    act(() => es.onopen?.(new Event("open")));
    expect(await screen.findByRole("button", { name: /● Live/ })).toBeInTheDocument();

    act(() => es.emit("log", mk(0, { message: "brand new", ts: 1_790_000_100_000 })));
    expect(await screen.findByText("brand new")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Pause" }));
    act(() => es.emit("log", mk(0, { message: "while paused", ts: 1_790_000_200_000 })));
    expect(await screen.findByRole("button", { name: /Resume \(1 waiting\)/ })).toBeInTheDocument();
    expect(screen.queryByText("while paused")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: /Resume/ }));
    expect(await screen.findByText("while paused")).toBeInTheDocument();

    act(() => es.emit("dropped", { dropped: 42 }));
    expect(await screen.findByRole("status")).toHaveTextContent("42 logs were skipped");

    await user.click(screen.getByRole("button", { name: /Live|Connecting/ }));
    await waitFor(() => expect(es.closed).toBe(true));
  });

  it("restores saved views and remembers a new one", async () => {
    mockApi();
    const mem = new Map<string, string>();
    vi.stubGlobal("localStorage", { getItem: (k: string) => mem.get(k) ?? null, setItem: (k: string, v: string) => void mem.set(k, v) });
    vi.spyOn(window, "prompt").mockReturnValue("my errors");
    const user = userEvent.setup();
    const router = renderAt("/logs?q=status%3Aerror&cols=route");
    await screen.findByRole("list", { name: "Logs" });
    await user.click(screen.getByRole("button", { name: "Save view" }));
    await user.click(await screen.findByRole("button", { name: "Delete a saved view" }));
    // (the delete prompt answers "my errors" too: the view is gone)
    await waitFor(() => expect(screen.queryByRole("option", { name: "my errors" })).not.toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Save view" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "Saved views" }), "my errors");
    await waitFor(() => expect(params(router).get("q")).toBe("status:error"));
    expect(params(router).get("cols")).toBe("route");
    vi.restoreAllMocks();
  });

  it("completes a facet value in the query box without running a half-written query", async () => {
    const f = mockApi();
    const user = userEvent.setup();
    renderAt("/logs");
    const box = await screen.findByRole("combobox", { name: "Log query" });
    await screen.findByRole("list", { name: "Logs" });
    await waitFor(() => expect(calls(f, "/api/v1/logs/facets").length).toBeGreaterThan(0));
    await user.type(box, "service:w");
    await user.click(await screen.findByRole("option", { name: "service:web-api" }));
    expect(box).toHaveValue("service:web-api");
    expect(calls(f, "/api/v1/logs").every((u) => !u.searchParams.get("q"))).toBe(true);
  });
});
