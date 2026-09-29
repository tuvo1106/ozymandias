import { DEFAULT_EXPLORER_STATE, legacyParams, parseExplorerState, serializeExplorerState, type ExplorerState } from "./explorerState";
import { RANGE_PRESETS } from "./timeRange";

const parse = (qs: string) => parseExplorerState(new URLSearchParams(qs));
const legacy = (qs: string) => legacyParams(new URLSearchParams(qs));

describe("legacyParams", () => {
  // The server is the one translator; a second one here would be a second
  // place to decide what it refuses. So: exactly as sent, in M1's order.
  it("carries M1's parameters exactly as sent", () => {
    expect(legacy("agg=sum:other{*}%20%2B%20avg&by=Route&filter=canary,a:b}&metric=%20m&range=4h")).toBe(
      new URLSearchParams({ metric: " m", filter: "canary,a:b}", by: "Route", agg: "sum:other{*} + avg" }).toString(),
    );
  });

  it("is nothing without them, and leaves out empty ones", () => {
    expect(legacy("q=sum:x{*}&range=4h")).toBe("");
    expect(legacy("metric=m&filter=&by=")).toBe("metric=m");
  });
});

describe("parseExplorerState", () => {
  it("gives the default state for an empty URL", () => {
    expect(parse("")).toEqual(DEFAULT_EXPLORER_STATE);
  });

  it("reads every field", () => {
    expect(parse("q=sum:x{*}&range=15m&live=0")).toEqual({
      q: "sum:x{*}",
      legacy: "",
      range: { kind: "relative", preset: "15m" },
      live: false,
    });
  });

  it("keeps an M1 link's parameters for the server to translate", () => {
    expect(parse("metric=m&by=a&range=4h")).toMatchObject({ q: "", legacy: "metric=m&by=a" });
  });

  it("prefers q to M1's parameters when a link has both", () => {
    expect(parse("q=sum:x{*}&metric=m")).toMatchObject({ q: "sum:x{*}", legacy: "" });
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

  it("writes q, and M1's parameters only until there is a q", () => {
    const legacyState = parse("metric=m&filter=!env:dev&agg=sum&from=1&to=2&live=0");
    expect(decodeURIComponent(serializeExplorerState(legacyState).toString())).toBe("metric=m&filter=!env:dev&agg=sum&from=1&to=2&live=0");
    expect(serializeExplorerState({ ...legacyState, q: "sum:m{*}", legacy: "" }).get("metric")).toBeNull();
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
      const q = rnd(4) === 0 ? "" : `${word()} ${word()}`;
      const s: ExplorerState = {
        q,
        legacy: q || rnd(2) === 0 ? "" : new URLSearchParams({ metric: word(), ...(rnd(2) ? { agg: word() } : {}) }).toString(),
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
