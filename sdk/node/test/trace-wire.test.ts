// Shared-vector tests for the pure trace wire logic: head sampling and the
// path normalizer must agree with Go (pkg/wire) on every vector, and span
// normalization must apply the limits the agent enforces.
import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  LIMITS,
  type WireSpan,
  normalizePath,
  normalizeWireSpan,
  parsePropagation,
  sampleKeep,
  truncBytes,
} from "../src/trace/wire.js";

const dir = new URL("../../../pkg/wire/testdata/traces/", import.meta.url);
const sampling = (JSON.parse(readFileSync(new URL("sampling.json", dir), "utf8")) as {
  cases: Array<{ trace_id: string; rate: number; keep: boolean }>;
}).cases;
const paths = (JSON.parse(readFileSync(new URL("normalize-path.json", dir), "utf8")) as {
  cases: Array<{ in: string; out: string }>;
}).cases;

describe("sampling vectors (sampling.json)", () => {
  it("loads a meaningful number of vectors", () => {
    expect(sampling.length).toBeGreaterThan(100);
    expect(new Set(sampling.map((c) => c.keep)).size).toBe(2);
  });
  it.each(sampling.map((c) => [`${c.trace_id.slice(16)} @ ${c.rate}`, c] as const))("%s", (_n, c) => {
    expect(sampleKeep(c.trace_id, c.rate)).toBe(c.keep);
  });
  it("never keeps on a malformed id (except at rate 1, which keeps everything)", () => {
    expect(sampleKeep("short", 0.5)).toBe(false);
    expect(sampleKeep("g".repeat(32), 0.5)).toBe(false);
    expect(sampleKeep("0".repeat(16) + "ABCDEF0123456789", 0.5)).toBe(false);
    expect(sampleKeep("x", 1)).toBe(true);
    expect(sampleKeep("x", NaN)).toBe(false);
    expect(sampleKeep("0".repeat(31) + "1", 1e-30)).toBe(false);
  });
});

describe("path normalizer vectors (normalize-path.json)", () => {
  it("loads vectors", () => expect(paths.length).toBeGreaterThan(20));
  it.each(paths.map((c) => [JSON.stringify(c.in), c] as const))("%s", (_n, c) => {
    expect(normalizePath(c.in)).toBe(c.out);
  });
  it("handles a path without a leading slash like Go does", () => {
    expect(normalizePath("api/42")).toBe("/api/:id");
  });
});

describe("parsePropagation", () => {
  const tid = "0123456789abcdef0123456789abcdef";
  const pid = "fedcba9876543210";
  it("accepts valid ids, lowercases and trims, defaults the priority to keep", () => {
    expect(parsePropagation(` ${tid.toUpperCase()} `, pid, undefined)).toEqual({ traceId: tid, parentId: pid, priority: 1 });
    expect(parsePropagation(tid, pid, "")?.priority).toBe(1);
    for (const p of ["-1", "0", "1", "2"]) expect(parsePropagation(tid, pid, p)?.priority).toBe(Number(p));
  });
  it.each([
    ["short trace id", "abc", pid, "1"],
    ["zero trace id", "0".repeat(32), pid, "1"],
    ["zero parent id", tid, "0".repeat(16), "1"],
    ["non-hex", "z".repeat(32), pid, "1"],
    ["priority out of range", tid, pid, "3"],
    ["priority garbage", tid, pid, "high"],
    ["non-string trace id", 5, pid, "1"],
    ["non-string parent", tid, null, "1"],
  ])("rejects %s", (_n, t, p, prio) => {
    expect(parsePropagation(t, p, prio)).toBeNull();
  });
});

function span(over: Partial<WireSpan> = {}): WireSpan {
  return {
    trace_id: "1".repeat(32),
    span_id: "2".repeat(16),
    parent_id: null,
    service: "svc",
    name: "op",
    resource: "res",
    type: "web",
    start: 1_700_000_000_000_000,
    duration: 5,
    error: 0,
    meta: {},
    metrics: {},
    ...over,
  };
}

describe("normalizeWireSpan (mirrors wire.NormalizeSpan / ValidateSpan)", () => {
  it("turns an unknown type into custom and keeps known ones", () => {
    expect(normalizeWireSpan(span({ type: "banana" })).type).toBe("custom");
    for (const t of ["web", "db", "cache", "queue", "http", "worker", "custom"]) expect(normalizeWireSpan(span({ type: t })).type).toBe(t);
  });
  it("cuts resource and meta values to 5000 bytes on a rune boundary", () => {
    const s = normalizeWireSpan(span({ resource: "é".repeat(4000), meta: { k: "😀".repeat(2000) } }));
    expect(Buffer.byteLength(s.resource)).toBeLessThanOrEqual(LIMITS.maxResourceBytes);
    expect(s.resource).toBe("é".repeat(2500));
    expect(Buffer.byteLength(s.meta["k"]!)).toBeLessThanOrEqual(LIMITS.maxMetaValueBytes);
    expect(s.meta["k"]).toBe("😀".repeat(1250));
    expect(s.resource.includes("�")).toBe(false);
  });
  it("keeps at most 100 meta and 50 metrics, dropping the rest in sorted key order", () => {
    const meta: Record<string, string> = {};
    for (let i = 0; i < 150; i++) meta[`k${String(i).padStart(3, "0")}`] = "v";
    const metrics: Record<string, number> = {};
    for (let i = 0; i < 80; i++) metrics[`m${String(i).padStart(3, "0")}`] = i;
    const s = normalizeWireSpan(span({ meta, metrics }));
    expect(Object.keys(s.meta)).toHaveLength(100);
    expect(Object.keys(s.metrics)).toHaveLength(50);
    expect(s.meta["k099"]).toBe("v");
    expect(s.meta["k100"]).toBeUndefined();
    expect(s.metrics["m049"]).toBe(49);
    expect(s.metrics["m050"]).toBeUndefined();
  });
  it("drops over-long and empty keys and non-finite metrics", () => {
    const s = normalizeWireSpan(span({ meta: { ["k".repeat(101)]: "v", "": "x", ok: "1" }, metrics: { nan: NaN, inf: Infinity, fine: 1 } }));
    expect(Object.keys(s.meta)).toEqual(["ok"]);
    expect(s.metrics).toEqual({ fine: 1 });
  });
  it("truncates service and name to 100 bytes and fills empty ones instead of being refused", () => {
    const s = normalizeWireSpan(span({ service: "s".repeat(150), name: "n".repeat(150) }));
    expect(s.service).toHaveLength(100);
    expect(s.name).toHaveLength(100);
    const e = normalizeWireSpan(span({ service: "", name: "" }));
    expect([e.service, e.name]).toEqual(["unknown", "unnamed"]);
  });
  it("coerces a bad error flag and a bad duration", () => {
    expect(normalizeWireSpan(span({ error: 7 as 1 })).error).toBe(0);
    expect(normalizeWireSpan(span({ duration: -5 })).duration).toBe(0);
    expect(normalizeWireSpan(span({ duration: NaN })).duration).toBe(0);
  });
});

describe("truncBytes", () => {
  it("leaves short strings alone and never splits a code point", () => {
    expect(truncBytes("abc", 10)).toBe("abc");
    expect(truncBytes("é".repeat(10), 5)).toBe("éé");
    expect(truncBytes("😀😀", 5)).toBe("😀");
    expect(truncBytes("a".repeat(100), 100)).toBe("a".repeat(100));
  });
});
