import {
  fromLocalInputValue,
  isRangePreset,
  presetSeconds,
  RANGE_PRESETS,
  resolveTimeRange,
  toLocalInputValue,
} from "./timeRange";

const NOW_MS = 1_790_003_600_500; // half a second past a whole second

describe("resolveTimeRange", () => {
  it.each([
    ["5m", 300],
    ["15m", 900],
    ["1h", 3600],
    ["4h", 14400],
    ["1d", 86400],
  ] as const)("resolves %s to the %i seconds ending now", (preset, secs) => {
    expect(resolveTimeRange({ kind: "relative", preset }, NOW_MS)).toEqual({
      from: 1_790_003_600 - secs,
      to: 1_790_003_600,
    });
    expect(presetSeconds(preset)).toBe(secs);
  });

  it("floors now to a whole second so repeat resolutions agree", () => {
    const a = resolveTimeRange({ kind: "relative", preset: "1h" }, 1_790_000_000_001);
    const b = resolveTimeRange({ kind: "relative", preset: "1h" }, 1_790_000_000_999);
    expect(a).toEqual(b);
  });

  it("returns absolute ranges unchanged, whatever the time", () => {
    expect(resolveTimeRange({ kind: "absolute", from: 10, to: 20 }, NOW_MS)).toEqual({ from: 10, to: 20 });
  });
});

describe("isRangePreset", () => {
  it("accepts every preset and nothing else", () => {
    for (const p of RANGE_PRESETS) expect(isRangePreset(p)).toBe(true);
    for (const s of ["", "2h", "1H", "custom"]) expect(isRangePreset(s)).toBe(false);
  });
});

describe("datetime-local conversion", () => {
  it("round-trips minute-aligned instants in the local zone", () => {
    for (const t of [0, 1_790_000_040, 1_790_003_580, 1_700_000_000 - 20]) {
      const aligned = t - (t % 60);
      expect(fromLocalInputValue(toLocalInputValue(aligned))).toBe(aligned);
    }
  });

  it("formats with zero padding", () => {
    expect(toLocalInputValue(new Date(2026, 0, 2, 3, 4).getTime() / 1000)).toBe("2026-01-02T03:04");
  });

  it("accepts an optional seconds field", () => {
    expect(fromLocalInputValue("2026-01-02T03:04:05")).toBe(new Date(2026, 0, 2, 3, 4, 5).getTime() / 1000);
  });

  it.each(["", "yesterday", "2026-01-02", "2026-02-31T00:00", "2026-13-01T00:00"])("rejects %j", (v) => {
    expect(fromLocalInputValue(v)).toBeUndefined();
  });
});
