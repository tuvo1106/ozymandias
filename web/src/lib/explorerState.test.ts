import {
  AGGREGATORS,
  DEFAULT_EXPLORER_STATE,
  formatFilter,
  parseExplorerState,
  parseFilter,
  serializeExplorerState,
  type ExplorerState,
} from "./explorerState";
import { RANGE_PRESETS } from "./timeRange";

const parse = (qs: string) => parseExplorerState(new URLSearchParams(qs));

describe("parseFilter / formatFilter", () => {
  it.each([
    ["env:dev", { key: "env", value: "dev", negate: false }],
    ["!env:dev", { key: "env", value: "dev", negate: true }],
    ["route:/api/*", { key: "route", value: "/api/*", negate: false }],
    ["url:http://x:8080", { key: "url", value: "http://x:8080", negate: false }],
    [" env:dev ", { key: "env", value: "dev", negate: false }],
  ])("parses %j", (term, want) => {
    expect(parseFilter(term)).toEqual(want);
  });

  it.each(["env", ":dev", "env:", "!", "!:x", ""])("rejects %j", (term) => {
    expect(parseFilter(term)).toBeUndefined();
  });

  it("formats the inverse of parse", () => {
    for (const t of ["env:dev", "!env:dev", "url:http://x:8080"]) {
      expect(formatFilter(parseFilter(t)!)).toBe(t);
    }
  });
});

describe("parseExplorerState", () => {
  it("gives the default state for an empty URL", () => {
    expect(parse("")).toEqual(DEFAULT_EXPLORER_STATE);
  });

  it("reads every field", () => {
    expect(parse("metric=http.request.count&filter=env:dev,!route:/x&by=route,env&agg=max&range=15m&live=0")).toEqual({
      metric: "http.request.count",
      filters: [
        { key: "env", value: "dev", negate: false },
        { key: "route", value: "/x", negate: true },
      ],
      by: ["route", "env"],
      agg: "max",
      range: { kind: "relative", preset: "15m" },
      live: false,
    });
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
  ])(
    "falls back to the preset for a bad absolute range (%s)",
    (qs) => {
      expect(parse(`${qs}&range=4h`).range).toEqual({ kind: "relative", preset: "4h" });
    },
  );

  it("degrades bad fields one at a time", () => {
    const s = parse("metric=m&agg=median&range=2h&filter=bad,env:dev,,&by=,a,,a,b");
    expect(s.metric).toBe("m");
    expect(s.agg).toBe("avg");
    expect(s.range).toEqual({ kind: "relative", preset: "1h" });
    expect(s.filters).toEqual([{ key: "env", value: "dev", negate: false }]);
    expect(s.by).toEqual(["a", "b"]);
  });

  it("drops duplicate filters but keeps a filter and its negation", () => {
    expect(parse("filter=env:dev,env:dev,!env:dev").filters).toHaveLength(2);
  });
});

describe("serializeExplorerState", () => {
  it("writes nothing for the default state", () => {
    expect(serializeExplorerState(DEFAULT_EXPLORER_STATE).toString()).toBe("");
  });

  it("uses the API's spelling", () => {
    const s: ExplorerState = {
      metric: "m",
      filters: [{ key: "env", value: "dev", negate: true }],
      by: ["a", "b"],
      agg: "sum",
      range: { kind: "absolute", from: 1, to: 2 },
      live: false,
    };
    expect(decodeURIComponent(serializeExplorerState(s).toString())).toBe(
      "metric=m&filter=!env:dev&by=a,b&agg=sum&from=1&to=2&live=0",
    );
  });

  // A small hand-rolled property test: random states survive the trip
  // through the URL unchanged.
  it("round-trips random states", () => {
    let seed = 42;
    const rnd = (n: number) => {
      seed = (seed * 1_103_515_245 + 12_345) % 2 ** 31;
      return seed % n;
    };
    const word = () => ["env", "route", "a.b", "x-y", "/api/*", "http://h:1", "ünï", "a b", "&=?"][rnd(9)]!;
    for (let i = 0; i < 500; i++) {
      const filters = new Map<string, { key: string; value: string; negate: boolean }>();
      for (let j = rnd(4); j > 0; j--) {
        const f = { key: word().replace(/[:,]/g, "") || "k", value: word(), negate: rnd(2) === 1 };
        filters.set(formatFilter(f), f);
      }
      const from = rnd(1_000_000);
      const s: ExplorerState = {
        metric: rnd(4) === 0 ? "" : `metric.${word()}`,
        filters: [...filters.values()],
        by: [...new Set(Array.from({ length: rnd(3) }, () => word().replace(/,/g, "")))],
        agg: AGGREGATORS[rnd(AGGREGATORS.length)]!,
        range:
          rnd(3) === 0
            ? { kind: "absolute", from, to: from + 1 + rnd(1000) }
            : { kind: "relative", preset: RANGE_PRESETS[rnd(RANGE_PRESETS.length)]! },
        live: rnd(2) === 0,
      };
      const url = `?${serializeExplorerState(s).toString()}`;
      expect(parseExplorerState(new URLSearchParams(url))).toEqual(s);
    }
  });
});
