import { fetchFacets, fetchHistogram, fetchLogs, openTail, type TailSource } from "./logsApi";

const reply = (body: unknown, status = 200) => vi.fn(async () => new Response(JSON.stringify(body), { status })) as unknown as typeof fetch;
const Q = { q: "service:web-api", from: 1000, to: 2000 };
const entry = { ts: 5, message: "m", status: "info", service: "s", attrs: { a: 1 } };

describe("fetchLogs", () => {
  it("sends the window in ms, the query, limit and cursor, and reads a page", async () => {
    const f = reply({ logs: [entry], cursor: "c1", truncated: true, stats: { streams: 1 } });
    const page = await fetchLogs(Q, { limit: 50, cursor: "c0" }, f);
    const url = new URL((f as unknown as { mock: { calls: string[][] } }).mock.calls[0]![0]!, "http://x");
    expect(Object.fromEntries(url.searchParams)).toEqual({ q: "service:web-api", from: "1000", to: "2000", limit: "50", cursor: "c0" });
    expect(page).toMatchObject({ cursor: "c1", truncated: true });
    expect(page.logs).toEqual([entry]);
  });
  it("omits q when empty and reads a last page", async () => {
    const f = reply({ logs: [] });
    const page = await fetchLogs({ ...Q, q: "" }, {}, f);
    expect(new URL((f as unknown as { mock: { calls: string[][] } }).mock.calls[0]![0]!, "http://x").searchParams.has("q")).toBe(false);
    expect(page).toEqual({ logs: [], cursor: undefined, truncated: false, stats: undefined });
  });
  it.each([[{}], [{ logs: [{ ts: "x" }] }], [{ logs: [{ ...entry, attrs: [] }] }], [[]]])("rejects a drifted body %j", async (body) => {
    await expect(fetchLogs(Q, {}, reply(body))).rejects.toThrow(/unexpected \/api\/v1\/logs response/);
  });
  it("surfaces the server's own error", async () => {
    await expect(fetchLogs(Q, {}, reply({ error: "col 3: bad" }, 400))).rejects.toThrow("col 3: bad");
  });
});

describe("fetchHistogram and fetchFacets", () => {
  it("reads a histogram", async () => {
    const h = await fetchHistogram(Q, "status", reply({ interval_ms: 1000, buckets: [{ ts: 1, counts: { info: 2 } }], truncated: false }));
    expect(h.buckets[0]!.counts).toEqual({ info: 2 });
  });
  it("rejects a bad histogram", async () => {
    await expect(fetchHistogram(Q, "status", reply({ interval_ms: 1, buckets: [{ ts: 1 }] }))).rejects.toThrow(/aggregate/);
    await expect(fetchHistogram(Q, "status", reply({}))).rejects.toThrow(/aggregate/);
  });
  it("reads facets and rejects bad ones", async () => {
    const f = reply({ facets: { status: [{ value: "info", count: 3 }] }, capped: ["x", 1], truncated: true });
    const out = await fetchFacets(Q, ["status", "service"], f);
    expect(out).toEqual({ facets: { status: [{ value: "info", count: 3 }] }, capped: ["x"], truncated: true });
    expect(new URL((f as unknown as { mock: { calls: string[][] } }).mock.calls[0]![0]!, "http://x").searchParams.get("keys")).toBe("status,service");
    await expect(fetchFacets(Q, ["a"], reply({ facets: { a: [{ value: 1 }] } }))).rejects.toThrow(/facets/);
    await expect(fetchFacets(Q, ["a"], reply({ facets: { a: 5 } }))).rejects.toThrow(/facets/);
    await expect(fetchFacets(Q, ["a"], reply({}))).rejects.toThrow(/facets/);
  });
});

describe("openTail", () => {
  function fake() {
    const listeners = new Map<string, (e: MessageEvent) => void>();
    const src: TailSource & { url: string; closed: boolean } = {
      url: "", closed: false, onerror: null, onopen: null,
      addEventListener: (t, fn) => void listeners.set(t, fn),
      close() { this.closed = true; },
    };
    return { src, emit: (t: string, data: string) => listeners.get(t)!({ data } as MessageEvent) };
  }
  it("delivers logs and drops, ignores junk, reports open/error, and closes", () => {
    const { src, emit } = fake();
    const got: unknown[] = [];
    const dropped: number[] = [];
    let opens = 0, errors = 0;
    let url = "";
    const close = openTail("status:error x", { onLog: (l) => got.push(l), onDropped: (n) => dropped.push(n), onOpen: () => opens++, onError: () => errors++ }, (u) => { url = u; return src; });
    expect(url).toBe("/api/v1/logs/tail?q=status%3Aerror+x");
    emit("log", JSON.stringify(entry));
    emit("log", "{not json");
    emit("log", JSON.stringify({ nope: 1 }));
    emit("dropped", '{"dropped":42}');
    emit("dropped", "junk");
    emit("dropped", '{"dropped":"x"}');
    src.onopen!(new Event("open"));
    src.onerror!(new Event("error"));
    expect(got).toEqual([entry]);
    expect(dropped).toEqual([42]);
    expect([opens, errors]).toEqual([1, 1]);
    close();
    expect(src.closed).toBe(true);
  });
  it("omits q when there is no filter", () => {
    const { src } = fake();
    let url = "";
    openTail("", { onLog() {}, onDropped() {}, onOpen() {}, onError() {} }, (u) => { url = u; return src; });
    expect(url).toBe("/api/v1/logs/tail");
  });
});
