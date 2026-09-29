import { DEFAULT_EXPLORER_STATE, legacyQuery, parseExplorerState, serializeExplorerState, type ExplorerState } from "./explorerState";
import { RANGE_PRESETS } from "./timeRange";

const parse = (qs: string) => parseExplorerState(new URLSearchParams(qs));
const legacy = (qs: string) => legacyQuery(new URLSearchParams(qs));

describe("legacyQuery", () => {
  // The same query the server builds from these parameters (api.md), so an
  // M1 link charts what it always charted.
  it.each([
    ["metric=m", "avg:m{*}"],
    ["metric=http.request.count&filter=env:dev,!route:/x&by=route,env&agg=max", "max:http.request.count{env:dev,!route:/x} by {route,env}"],
    ["metric=m&filter=route:/api/*,url:http://x:8080", "avg:m{route:/api/*,url:http://x:8080}"],
    ["metric=m&filter=%20env:dev%20", "avg:m{env:dev}"],
  ])("translates %s", (qs, want) => {
    expect(legacy(qs)).toBe(want);
  });

  it("degrades bad fields one at a time, as M1 did", () => {
    expect(legacy("metric=m&agg=median&filter=bad,:x,env:,env:dev,,env:dev&by=,a,,a,b")).toBe("avg:m{env:dev} by {a,b}");
  });

  it("is nothing without a metric", () => {
    expect(legacy("filter=env:dev&by=a&agg=sum")).toBe("");
    expect(legacy("metric=%20")).toBe("");
  });

  // Dropping the brace would chart a different query from the one the link
  // names; keeping it lets the query box say why it does not parse.
  it("keeps what the parser will refuse rather than rewriting it", () => {
    expect(legacy("metric=m&filter=a:b}")).toBe("avg:m{a:b}}");
  });
});

describe("parseExplorerState", () => {
  it("gives the default state for an empty URL", () => {
    expect(parse("")).toEqual(DEFAULT_EXPLORER_STATE);
  });

  it("reads every field", () => {
    expect(parse("q=sum:x{*}&range=15m&live=0")).toEqual({
      q: "sum:x{*}",
      range: { kind: "relative", preset: "15m" },
      live: false,
    });
  });

  it("reads an M1 link as the query it asked for", () => {
    expect(parse("metric=m&by=a&range=4h").q).toBe("avg:m{*} by {a}");
  });

  it("prefers q to M1's parameters when a link has both", () => {
    expect(parse("q=sum:x{*}&metric=m").q).toBe("sum:x{*}");
  });

  it("prefers a valid absolute range over a preset", () => {
    expect(parse("range=5m&from=100&to=200").range).toEqual({ kind: "absolute", from: 100, to: 200 });
  });

  // "from=&to=200" is the regression: Number("") is 0, so a blank value used
  // to parse as a valid absolute range starting at the epoch.
  it.each([
    "from=200&to=100",
    "from=100",
    "from=a&to=200",
    "from=1.5&to=200",
    "from=100&to=100",
    "from=&to=200",
    "from=100&to=",
    "from=%20&to=200",
  ])("falls back to the preset for a bad absolute range (%s)", (qs) => {
    expect(parse(`${qs}&range=4h`).range).toEqual({ kind: "relative", preset: "4h" });
  });

  it("falls back to the default preset for an unknown one", () => {
    expect(parse("range=2h").range).toEqual({ kind: "relative", preset: "1h" });
  });
});

describe("serializeExplorerState", () => {
  it("writes nothing for the default state", () => {
    expect(serializeExplorerState(DEFAULT_EXPLORER_STATE).toString()).toBe("");
  });

  it("writes q, never M1's parameters", () => {
    const s = parse("metric=m&filter=!env:dev&agg=sum&from=1&to=2&live=0");
    expect(decodeURIComponent(serializeExplorerState(s).toString())).toBe("q=sum:m{!env:dev}&from=1&to=2&live=0");
  });

  // A small hand-rolled property test: random states survive the trip
  // through the URL unchanged.
  it("round-trips random states", () => {
    let seed = 42;
    const rnd = (n: number) => {
      seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
      return seed % n;
    };
    const word = () => ["sum:x{*}", "a b", "&=?", "ünï", "p95:y{r:/a/*} by {k}", "x{a:b}}", "#h", "+"][rnd(8)]!;
    for (let i = 0; i < 500; i++) {
      const from = rnd(1_000_000);
      const s: ExplorerState = {
        q: rnd(4) === 0 ? "" : `${word()} ${word()}`,
        range:
          rnd(3) === 0
            ? { kind: "absolute", from, to: from + 1 + rnd(1000) }
            : { kind: "relative", preset: RANGE_PRESETS[rnd(RANGE_PRESETS.length)]! },
        live: rnd(2) === 0,
      };
      expect(parseExplorerState(new URLSearchParams(`?${serializeExplorerState(s).toString()}`))).toEqual(s);
    }
  });
});
