import type { LogEntry } from "./logsApi";
import { addTerm, attrText, completions, deleteView, flattenAttrs, loadViews, mergeTail, quoteValue, rowKey, saveView, statusColor, visibleRange } from "./logsView";

const log = (ts: number, message = "m", extra: Partial<LogEntry> = {}): LogEntry => ({ ts, message, status: "info", service: "web-api", ...extra });

describe("addTerm", () => {
  it("adds labels bare and attributes with @", () => {
    expect(addTerm("", "service", "web-api")).toBe("service:web-api");
    expect(addTerm("timeout", "route", "/orders")).toBe("timeout @route:/orders");
    expect(addTerm("a", "status", "error", true)).toBe("a -status:error");
  });
  it("does not repeat a term and quotes what needs it", () => {
    expect(addTerm("service:web-api", "service", "web-api")).toBe("service:web-api");
    expect(addTerm("", "user", "Jane Doe")).toBe('@user:"Jane Doe"');
    expect(addTerm("", "@user", "x")).toBe("@user:x");
  });
});

describe("quoteValue", () => {
  it.each([
    ["plain", "plain"],
    ["/api/orders", "/api/orders"],
    ["has space", '"has space"'],
    ['say "hi"', '"say \\"hi\\""'],
    ["a\nb", '"a\\nb"'],
    ["back\\slash", '"back\\\\slash"'],
    ["-neg", '"-neg"'],
    ["a:b", '"a:b"'],
    ["", '""'],
  ])("%j", (v, want) => expect(quoteValue(v)).toBe(want));
});

describe("attributes", () => {
  const l = log(1, "m", { attrs: { b: 1, a: { c: "x", d: { e: true } }, arr: [1, 2] } });
  it("flattens nested objects to sorted dotted paths, arrays stay values", () => {
    expect(flattenAttrs(l.attrs)).toEqual([["a.c", "x"], ["a.d.e", true], ["arr", [1, 2]], ["b", 1]]);
    expect(flattenAttrs(undefined)).toEqual([]);
  });
  it("reads a value as text", () => {
    expect(attrText(l, "a.c")).toBe("x");
    expect(attrText(l, "b")).toBe("1");
    expect(attrText(l, "arr")).toBe("[1,2]");
    expect(attrText(l, "nope")).toBe("");
  });
});

describe("mergeTail", () => {
  it("puts arrivals above the list, newest first, once", () => {
    const list = [log(3), log(2)];
    const out = mergeTail(list, [log(4, "a"), log(5, "b"), log(3)], 100);
    expect(out.map((l) => l.ts)).toEqual([5, 4, 3, 2]);
  });
  it("caps the list, dropping the oldest", () => {
    expect(mergeTail([log(1), log(2)], [log(3), log(4)], 3).map((l) => l.ts)).toEqual([4, 3, 1]);
  });
  it("does not collapse distinct logs that share a timestamp", () => {
    expect(mergeTail([], [log(1, "a"), log(1, "b")], 10)).toHaveLength(2);
    expect(rowKey(log(1, "a"))).not.toBe(rowKey(log(1, "b")));
  });
});

describe("visibleRange", () => {
  it("renders the window plus overscan, clamped", () => {
    expect(visibleRange(0, 100, 10, 1000, 2)).toEqual({ start: 0, end: 12 });
    expect(visibleRange(500, 100, 10, 1000, 2)).toEqual({ start: 48, end: 62 });
    expect(visibleRange(9990, 100, 10, 1000, 2)).toEqual({ start: 997, end: 1000 });
    expect(visibleRange(0, 100, 10, 0, 2)).toEqual({ start: 0, end: 0 });
  });
});

describe("statusColor", () => {
  it("is distinct per known status and neutral otherwise", () => {
    const known = ["debug", "info", "warn", "error", "critical"].map(statusColor);
    expect(new Set(known).size).toBe(5);
    expect(statusColor("weird")).toBe("bg-zinc-500");
  });
});

describe("completions", () => {
  const facets = { service: [{ value: "web-api" }, { value: "worker" }, { value: "has space" }], status: [{ value: "error" }] };
  it("completes a key", () => {
    expect(completions("ser", facets)).toEqual(["service:"]);
    expect(completions("timeout -st", facets)).toEqual(["timeout -status:"]);
  });
  it("completes a value from the facets, quoting as needed, keeping the rest", () => {
    expect(completions("status:error x service:w", facets)).toEqual(["status:error x service:web-api", "status:error x service:worker"]);
    expect(completions("service:ha", facets)).toEqual(['service:"has space"']);
  });
  it("offers nothing for a finished token, an attribute key, or a trailing space", () => {
    expect(completions("service:web-api", facets)).toEqual([]);
    expect(completions("status", facets)).toEqual([]);
    expect(completions("@ro", facets)).toEqual([]);
    expect(completions("service:worker ", facets)).toEqual([]);
    expect(completions("", facets)).toEqual([]);
  });
});

describe("saved views", () => {
  const mem = () => {
    const m = new Map<string, string>();
    return { getItem: (k: string) => m.get(k) ?? null, setItem: (k: string, v: string) => void m.set(k, v) };
  };
  it("saves, replaces by name and deletes", () => {
    const s = mem();
    saveView({ name: "errors", search: "q=a" }, s);
    saveView({ name: "slow", search: "q=b" }, s);
    saveView({ name: "errors", search: "q=c" }, s);
    expect(loadViews(s)).toEqual([{ name: "slow", search: "q=b" }, { name: "errors", search: "q=c" }]);
    expect(deleteView("slow", s)).toEqual([{ name: "errors", search: "q=c" }]);
  });
  it("treats junk and a throwing store as empty", () => {
    expect(loadViews({ getItem: () => "{nope" })).toEqual([]);
    expect(loadViews({ getItem: () => '[{"name":1},{"name":"ok","search":""}]' })).toEqual([{ name: "ok", search: "" }]);
    expect(loadViews({ getItem: () => '{"a":1}' })).toEqual([]);
    const throwing = { getItem: () => null, setItem: () => { throw new Error("full"); } };
    expect(saveView({ name: "x", search: "" }, throwing)).toEqual([{ name: "x", search: "" }]);
    expect(deleteView("x", throwing)).toEqual([]);
  });
});
