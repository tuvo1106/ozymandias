import { DEFAULT_LOGS_STATE, parseLogsState, serializeLogsState, type LogsState } from "./logsState";

const parse = (s: string) => parseLogsState(new URLSearchParams(s));

describe("logs state", () => {
  it("defaults", () => {
    expect(parse("")).toEqual(DEFAULT_LOGS_STATE);
    expect(serializeLogsState(DEFAULT_LOGS_STATE).toString()).toBe("");
  });
  it("round-trips every field", () => {
    const states: LogsState[] = [
      { q: "service:web-api status:error", range: { kind: "relative", preset: "4h" }, tail: true, cols: ["route", "user.id"] },
      { q: "", range: { kind: "absolute", from: 10, to: 20 }, tail: false, cols: [] },
    ];
    for (const s of states) expect(parse(serializeLogsState(s).toString())).toEqual(s);
  });
  it("degrades hand-edited values to defaults", () => {
    expect(parse("range=9y").range).toEqual(DEFAULT_LOGS_STATE.range);
    expect(parse("from=&to=").range).toEqual(DEFAULT_LOGS_STATE.range);
    expect(parse("from=20&to=10").range).toEqual(DEFAULT_LOGS_STATE.range);
    expect(parse("from=1.5&to=9").range).toEqual(DEFAULT_LOGS_STATE.range);
    expect(parse("tail=yes").tail).toBe(false);
    expect(parse("cols=a,,a, b ,").cols).toEqual(["a", "b"]);
    expect(parse("q=%20x%20").q).toBe("x");
  });
});
