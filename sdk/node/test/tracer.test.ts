// Tracer core: span lifecycle, context, buffering, sampling, rate uptake,
// propagation, and the guarantee that the host's code runs and fails unchanged.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { NOOP_SPAN, init, tracer } from "../src/index.js";
import { globalState } from "../src/state.js";
import { PARTIAL_FLUSH_SPANS, newId, setEntropyForTest } from "../src/trace/tracer.js";
import { sampleKeep } from "../src/trace/wire.js";
import { clearEnv } from "./helpers.js";
import { FakeTraceAgent, initTracing, resetTracing } from "./trace-helpers.js";

let agent: FakeTraceAgent;
let restoreEnv: () => void;

beforeAll(async () => {
  agent = await FakeTraceAgent.start();
});
afterAll(async () => {
  await agent.close();
});
beforeEach(() => {
  restoreEnv = clearEnv();
  agent.clear();
  agent.status = 200;
  agent.rates = {};
  initTracing(agent);
});
afterEach(async () => {
  setEntropyForTest();
  await resetTracing();
  restoreEnv();
});

const HEX32 = /^[0-9a-f]{32}$/;
const HEX16 = /^[0-9a-f]{16}$/;

describe("span lifecycle", () => {
  it("emits a well-formed root span with defaults from init", async () => {
    const before = Date.now() * 1000;
    const result = await tracer.trace("op", { resource: "GET /x", type: "web" }, async (span) => {
      span.setTag("k", "v").setTag("n", 5).setMetric("m", 2.5);
      return 42;
    });
    const after = Date.now() * 1000;
    expect(result).toBe(42);
    await tracer.flush();
    const [s] = agent.spans();
    expect(s).toBeDefined();
    expect(s!.trace_id).toMatch(HEX32);
    expect(s!.span_id).toMatch(HEX16);
    expect(s!.parent_id).toBeNull();
    expect([s!.service, s!.name, s!.resource, s!.type, s!.error]).toEqual(["svc", "op", "GET /x", "web", 0]);
    expect(s!.start).toBeGreaterThanOrEqual(before - 2000);
    expect(s!.start).toBeLessThanOrEqual(after + 2000);
    expect(s!.duration).toBeGreaterThanOrEqual(0);
    expect(s!.meta).toMatchObject({ env: "test", version: "1.2.3", k: "v", n: "5" });
    expect(s!.metrics).toMatchObject({ m: 2.5, _top_level: 1, _sampling_priority: 1 });
    expect(agent.bodies[0]!.tracer).toMatchObject({ lang: "node" });
  });

  it("measures duration on the monotonic clock", async () => {
    await tracer.trace("slow", {}, () => new Promise((r) => setTimeout(r, 30)));
    await tracer.flush();
    const d = agent.spans()[0]!.duration;
    expect(d).toBeGreaterThanOrEqual(25_000);
    expect(d).toBeLessThan(2_000_000);
  });

  it("re-anchors the wall clock when it jumps (laptop sleep: the monotonic clock paused, the wall clock did not)", async () => {
    vi.useFakeTimers({ toFake: ["Date"], now: Date.now() });
    try {
      tracer.trace("before", {}, () => {});
      vi.setSystemTime(Date.now() + 3_600_000);
      const wallUs = Date.now() * 1000;
      tracer.trace("after", {}, () => {});
      await tracer.flush();
      const after = agent.spans().find((s) => s.name === "after")!;
      expect(Math.abs(after.start - wallUs)).toBeLessThan(100_000);
    } finally {
      vi.useRealTimers();
    }
  });

  it("defaults the resource to the name and unknown types to custom", async () => {
    tracer.trace("op", { type: "weird" }, () => {});
    await tracer.flush();
    expect(agent.spans()[0]).toMatchObject({ resource: "op", type: "custom" });
  });

  it("finish() is idempotent and a finished span ignores later edits", async () => {
    const span = tracer.startSpan("manual", { resource: "r" });
    span.setTag("a", "1");
    span.finish();
    span.finish();
    span.setTag("late", "x").setMetric("late", 1).setResource("changed").setError(new Error("late"));
    await tracer.flush();
    expect(agent.spans()).toHaveLength(1);
    expect(agent.spans()[0]).toMatchObject({ resource: "r", error: 0 });
    expect(agent.spans()[0]!.meta["late"]).toBeUndefined();
  });

  it("records a thrown error and re-throws the very same object", async () => {
    const boom = new TypeError("kaput");
    expect(() =>
      tracer.trace("sync", {}, () => {
        throw boom;
      }),
    ).toThrow(boom);
    await expect(tracer.trace("async", {}, async () => Promise.reject(boom))).rejects.toBe(boom);
    await tracer.flush();
    const spans = agent.spans();
    expect(spans).toHaveLength(2);
    for (const s of spans) {
      expect(s.error).toBe(1);
      expect(s.meta).toMatchObject({ "error.type": "TypeError", "error.message": "kaput" });
      expect(s.meta["error.stack"]).toContain("kaput");
    }
  });

  it("records a non-Error throw and survives a hostile error object", async () => {
    expect(() =>
      tracer.trace("str", {}, () => {
        throw "plain string";
      }),
    ).toThrow("plain string");
    const hostile = {
      get message(): string {
        throw new Error("getter");
      },
    };
    expect(() =>
      tracer.trace("hostile", {}, () => {
        throw hostile;
      }),
    ).toThrow();
    await tracer.flush();
    const byName = Object.fromEntries(agent.spans().map((s) => [s.name, s]));
    expect(byName["str"]).toMatchObject({ error: 1, meta: expect.objectContaining({ "error.type": "string", "error.message": "plain string" }) });
    expect(byName["hostile"]!.error).toBe(1);
  });

  it("accepts trace(name, fn) without options", async () => {
    expect(tracer.trace("bare", () => 7)).toBe(7);
    await tracer.flush();
    expect(agent.spans()[0]!.name).toBe("bare");
  });

  it("wrap() traces each call, forwards this and arguments, and preserves the result", async () => {
    const obj = {
      base: 10,
      add(this: { base: number }, n: number) {
        return this.base + n;
      },
    };
    const wrapped = tracer.wrap("add", { type: "custom" }, obj.add);
    expect(wrapped.call(obj, 5)).toBe(15);
    expect(wrapped.call(obj, 1)).toBe(11);
    await tracer.flush();
    expect(agent.spans().map((s) => s.name)).toEqual(["add", "add"]);
  });

  it("truncates what the agent would refuse instead of sending it", async () => {
    tracer.trace("n".repeat(300), { service: "s".repeat(300), resource: "r".repeat(9000), type: "nope" }, (span) => {
      span.setTag("k", "v".repeat(9000));
      span.setMetric("bad", NaN);
    });
    await tracer.flush();
    const s = agent.spans()[0]!;
    expect(s.name).toHaveLength(100);
    expect(s.service).toHaveLength(100);
    expect(s.resource).toHaveLength(5000);
    expect(s.meta["k"]).toHaveLength(5000);
    expect(s.metrics["bad"]).toBeUndefined();
    expect(s.type).toBe("custom");
  });
});

describe("ids", () => {
  it("are lowercase hex of the right length and never zero, even if entropy returns zeros", () => {
    let calls = 0;
    setEntropyForTest((n) => {
      calls++;
      // First pool is all zero; the second is random.
      return calls === 1 ? Buffer.alloc(n) : Buffer.alloc(n, 0xab);
    });
    const t = newId(16);
    const s = newId(8);
    expect(t).toMatch(HEX32);
    expect(s).toMatch(HEX16);
    expect(t).not.toBe("0".repeat(32));
    expect(calls).toBeGreaterThanOrEqual(1);
  });
  it("rejects a starved entropy source rather than looping", () => {
    setEntropyForTest(() => Buffer.alloc(2));
    expect(() => newId(8)).toThrow();
  });
  it("are unique across many spans", () => {
    const seen = new Set<string>();
    for (let i = 0; i < 20_000; i++) seen.add(newId(8));
    expect(seen.size).toBe(20_000);
  });
});

describe("context and nesting", () => {
  it("children share the trace, parent to the active span, and flush with the root as one chunk", async () => {
    await tracer.trace("root", { type: "web" }, async (root) => {
      await tracer.trace("child", {}, async (child) => {
        expect(tracer.scope().active()).toBe(child);
        await tracer.trace("grandchild", {}, async () => {});
      });
      expect(tracer.scope().active()).toBe(root);
    });
    expect(tracer.scope().active()).toBeNull();
    await tracer.flush();
    const chunks = agent.chunks();
    expect(chunks).toHaveLength(1);
    const by = Object.fromEntries(chunks[0]!.map((s) => [s.name, s]));
    expect(new Set(chunks[0]!.map((s) => s.trace_id)).size).toBe(1);
    expect(by["child"]!.parent_id).toBe(by["root"]!.span_id);
    expect(by["grandchild"]!.parent_id).toBe(by["child"]!.span_id);
    expect(by["root"]!.parent_id).toBeNull();
    // Only the entry span is `_top_level`: same-service children are not.
    expect(by["root"]!.metrics["_top_level"]).toBe(1);
    expect(by["child"]!.metrics["_top_level"]).toBeUndefined();
    // The sampling verdict is on the root only (and on the first span of a chunk without it).
    expect(by["root"]!.metrics["_sampling_priority"]).toBe(1);
  });

  it("a child in another service is top-level", async () => {
    tracer.trace("root", {}, () => {
      tracer.trace("other", { service: "db-proxy" }, () => {});
    });
    await tracer.flush();
    const other = agent.spans().find((s) => s.name === "other")!;
    expect(other.service).toBe("db-proxy");
    expect(other.metrics["_top_level"]).toBe(1);
  });

  it("1000 concurrent scopes never cross-parent", async () => {
    const N = 1000;
    const observed: Array<{ id: number; parent: string | undefined; me: string }> = [];
    await Promise.all(
      Array.from({ length: N }, (_v, i) =>
        tracer.trace(`root-${i}`, {}, async (root) => {
          await new Promise((r) => setTimeout(r, Math.random() * 20));
          await tracer.trace("child", { tags: { id: i } }, async (child) => {
            await new Promise((r) => setImmediate(r));
            const active = tracer.scope().active();
            observed.push({ id: i, parent: active === child ? root.spanId : undefined, me: child.spanId });
          });
        }),
      ),
    );
    await tracer.flush();
    expect(observed).toHaveLength(N);
    const spans = agent.spans();
    const rootsByName = new Map(spans.filter((s) => s.name.startsWith("root-")).map((s) => [s.name, s]));
    expect(rootsByName.size).toBe(N);
    for (const c of spans.filter((s) => s.name === "child")) {
      const root = rootsByName.get(`root-${c.meta["id"]}`)!;
      expect(c.parent_id).toBe(root.span_id);
      expect(c.trace_id).toBe(root.trace_id);
    }
    expect(new Set(spans.map((s) => s.trace_id)).size).toBe(N);
  });

  it("childOf overrides the active span; null forces a new root", async () => {
    const a = tracer.startSpan("a");
    tracer.trace("x", {}, () => {
      const detached = tracer.startSpan("detached", { childOf: null });
      const adopted = tracer.startSpan("adopted", { childOf: a });
      const fromCtx = tracer.startSpan("fromctx", { childOf: { traceId: "a".repeat(32), spanId: "b".repeat(16), priority: 0 } });
      const garbage = tracer.startSpan("garbage", { childOf: {} as never });
      const noop = tracer.startSpan("noopparent", { childOf: NOOP_SPAN });
      for (const s of [detached, adopted, fromCtx, garbage, noop]) s.finish();
    });
    a.finish();
    await tracer.flush();
    const by = Object.fromEntries(agent.spans().map((s) => [s.name, s]));
    expect(by["detached"]!.parent_id).toBeNull();
    expect(by["detached"]!.trace_id).not.toBe(by["x"]!.trace_id);
    expect(by["adopted"]!.parent_id).toBe(by["a"]!.span_id);
    expect(by["fromctx"]).toMatchObject({ trace_id: "a".repeat(32), parent_id: "b".repeat(16) });
    expect(by["fromctx"]!.metrics["_sampling_priority"]).toBe(0);
    expect(by["garbage"]!.parent_id).toBeNull();
    expect(by["noopparent"]!.parent_id).toBeNull();
  });

  it("scope().activate makes a manually started span the parent", async () => {
    const m = tracer.startSpan("manual");
    tracer.scope().activate(m, () => {
      tracer.trace("inner", {}, () => {});
    });
    m.finish();
    await tracer.flush();
    const by = Object.fromEntries(agent.spans().map((s) => [s.name, s]));
    expect(by["inner"]!.parent_id).toBe(by["manual"]!.span_id);
  });

  it("shares the active span between two copies of the package through global state", () => {
    tracer.trace("outer", {}, (span) => {
      // A second copy of the module reads the same ALS from the same global state.
      expect(globalState().als?.getStore()).toBe(span);
    });
  });
});

describe("two loaded copies of the module", () => {
  it("a span made by the other copy is still recognised as the parent (brand, not instanceof)", async () => {
    vi.resetModules();
    const other = await import("../src/trace/tracer.js");
    expect(other.tracer).not.toBe(tracer); // a genuinely second copy, with its own SpanImpl class
    let seen = "";
    other.tracer.trace("outer", {}, (outer) => {
      tracer.trace("inner", {}, (inner) => {
        seen = inner.traceId === outer.traceId ? "same trace" : "different trace";
      });
    });
    expect(seen).toBe("same trace");
  });
});

describe("buffer flush rules", () => {
  it("holds children until the local root finishes", async () => {
    const root = tracer.startSpan("root");
    tracer.startSpan("child", { childOf: root }).finish();
    await tracer.flush();
    expect(agent.spans()).toHaveLength(0);
    root.finish();
    await tracer.flush();
    expect(agent.chunks().map((c) => c.length)).toEqual([2]);
  });

  it("flushes a partial chunk past 500 finished spans, stamping the priority on its first span", async () => {
    const root = tracer.startSpan("root");
    for (let i = 0; i < PARTIAL_FLUSH_SPANS; i++) tracer.startSpan("c", { childOf: root }).finish();
    await tracer.flush();
    expect(agent.chunks()).toHaveLength(1);
    expect(agent.chunks()[0]).toHaveLength(PARTIAL_FLUSH_SPANS);
    expect(agent.chunks()[0]![0]!.metrics["_sampling_priority"]).toBe(1);
    expect(agent.spans().some((s) => s.name === "root")).toBe(false);
    root.finish();
    await tracer.flush();
    expect(agent.chunks().map((c) => c.length)).toEqual([PARTIAL_FLUSH_SPANS, 1]);
  });

  it("sends a span that finishes after its root as a late chunk", async () => {
    const root = tracer.startSpan("root");
    const late = tracer.startSpan("late", { childOf: root });
    root.finish();
    await tracer.flush();
    expect(agent.chunks().map((c) => c.map((s) => s.name))).toEqual([["root"]]);
    late.finish();
    await tracer.flush();
    expect(agent.chunks().map((c) => c.map((s) => s.name))).toEqual([["root"], ["late"]]);
    expect(agent.chunks()[1]![0]!.trace_id).toBe(agent.chunks()[0]![0]!.trace_id);
    expect(agent.chunks()[1]![0]!.metrics["_sampling_priority"]).toBe(1);
  });

  it("an abandoned span never pins or corrupts its trace", async () => {
    const root = tracer.startSpan("root");
    tracer.startSpan("never-finished", { childOf: root });
    root.finish();
    await tracer.flush();
    expect(agent.spans().map((s) => s.name)).toEqual(["root"]);
  });
});

describe("head sampling", () => {
  it("keeps everything at the default rate", async () => {
    for (let i = 0; i < 50; i++) tracer.trace("t", {}, () => {});
    await tracer.flush();
    expect(agent.spans().every((s) => s.metrics["_sampling_priority"] === 1)).toBe(true);
  });

  it("at rate 0 every trace is priority 0 but is STILL SENT (the agent needs 100% for stats)", async () => {
    initTracing(agent, { traceSampleRate: 0 });
    for (let i = 0; i < 20; i++) tracer.trace("t", {}, () => {});
    await tracer.flush();
    expect(agent.spans()).toHaveLength(20);
    expect(agent.spans().every((s) => s.metrics["_sampling_priority"] === 0)).toBe(true);
    expect(tracer.stats().unsampled).toBe(20);
  });

  it("the decision is exactly sampleKeep(trace_id, rate)", async () => {
    initTracing(agent, { traceSampleRate: 0.5 });
    for (let i = 0; i < 300; i++) tracer.trace("t", {}, () => {});
    await tracer.flush();
    const spans = agent.spans();
    for (const s of spans) expect(s.metrics["_sampling_priority"]).toBe(sampleKeep(s.trace_id, 0.5) ? 1 : 0);
    const kept = spans.filter((s) => s.metrics["_sampling_priority"] === 1).length;
    expect(kept).toBeGreaterThan(90);
    expect(kept).toBeLessThan(210);
  });

  it("takes the rate from OZY_TRACE_SAMPLE_RATE", async () => {
    process.env["OZY_TRACE_SAMPLE_RATE"] = "0";
    initTracing(agent);
    tracer.trace("t", {}, () => {});
    await tracer.flush();
    expect(agent.spans()[0]!.metrics["_sampling_priority"]).toBe(0);
  });

  it("an extracted context keeps the upstream priority, whatever the local rate", async () => {
    initTracing(agent, { traceSampleRate: 0 });
    const headers = { "x-ozy-trace-id": "c".repeat(32), "x-ozy-parent-id": "d".repeat(16), "x-ozy-sampling-priority": "2" };
    tracer.trace("downstream", { childOf: tracer.extract(headers) }, () => {});
    await tracer.flush();
    expect(agent.spans()[0]).toMatchObject({ trace_id: "c".repeat(32), parent_id: "d".repeat(16) });
    expect(agent.spans()[0]!.metrics["_sampling_priority"]).toBe(2);
  });

  it("applies the agent's rate_by_service to later traces, keyed service,env", async () => {
    agent.rates = { "service:svc,env:test": 0, "service:other,env:test": 1 };
    tracer.trace("first", {}, () => {});
    await tracer.flush();
    expect(agent.spans()[0]!.metrics["_sampling_priority"]).toBe(1);
    // The answer to that request carried the new rates.
    agent.clear();
    for (let i = 0; i < 10; i++) tracer.trace("after", {}, () => {});
    tracer.trace("other-svc", { service: "other" }, () => {});
    await tracer.flush();
    const spans = agent.spans();
    expect(spans.filter((s) => s.name === "after").every((s) => s.metrics["_sampling_priority"] === 0)).toBe(true);
    expect(spans.find((s) => s.name === "other-svc")!.metrics["_sampling_priority"]).toBe(1);
    // A rate that disappears from the agent's table reverts to the local default.
    agent.rates = {};
    tracer.trace("trigger", {}, () => {});
    await tracer.flush();
    agent.clear();
    tracer.trace("reverted", {}, () => {});
    await tracer.flush();
    expect(agent.spans()[0]!.metrics["_sampling_priority"]).toBe(1);
  });

  it("ignores junk in rate_by_service and clamps out-of-range rates", async () => {
    agent.rates = { "service:svc,env:test": 7, bad: "x" as unknown as number };
    tracer.trace("a", {}, () => {});
    await tracer.flush();
    agent.clear();
    tracer.trace("b", {}, () => {});
    await tracer.flush();
    expect(agent.spans()[0]!.metrics["_sampling_priority"]).toBe(1);
  });
});

describe("propagation", () => {
  it("extract(inject(ctx)) round-trips through a plain object, a Headers and a Request", () => {
    tracer.trace("caller", {}, (span) => {
      const plain = tracer.inject(span, {});
      expect(plain).toMatchObject({ "x-ozy-trace-id": span.traceId, "x-ozy-parent-id": span.spanId, "x-ozy-sampling-priority": "1" });
      const expected = { traceId: span.traceId, spanId: span.spanId, priority: 1 };
      expect(tracer.extract(plain)).toEqual(expected);
      const h = tracer.inject(span, new Headers());
      expect(tracer.extract(h)).toEqual(expected);
      expect(tracer.extract(new Request("http://x/", { headers: h }))).toEqual(expected);
      // inject() with no context uses the active span; contexts work too.
      expect(tracer.inject(undefined, {})).toEqual(plain);
      expect(tracer.extract(tracer.inject(span.context(), {}))).toEqual(expected);
    });
  });

  it("extract is case-insensitive, takes the first of an array, and lowercases ids", () => {
    const ctx = tracer.extract({
      "X-Ozy-Trace-Id": ["A".repeat(32)],
      "X-OZY-PARENT-ID": "B".repeat(16),
      "x-ozy-sampling-priority": "0",
    });
    expect(ctx).toEqual({ traceId: "a".repeat(32), spanId: "b".repeat(16), priority: 0 });
  });

  it("garbage yields no context and never throws", () => {
    const bad = [
      null,
      undefined,
      "string",
      42,
      {},
      { "x-ozy-trace-id": "nope" },
      { "x-ozy-trace-id": "0".repeat(32), "x-ozy-parent-id": "1".repeat(16) },
      { "x-ozy-trace-id": "1".repeat(32), "x-ozy-parent-id": "1".repeat(16), "x-ozy-sampling-priority": "99" },
      { get: () => { throw new Error("boom"); } },
      { headers: { get: () => 5 } },
    ];
    for (const b of bad) expect(tracer.extract(b as never)).toBeNull();
  });

  it("inject never throws on a hostile carrier, and skips when there is nothing to inject", () => {
    const frozen = Object.freeze({});
    expect(() => tracer.inject(tracer.startSpan("s"), frozen)).not.toThrow();
    const empty: Record<string, string> = {};
    expect(tracer.inject(undefined, empty)).toEqual({});
    expect(tracer.inject(NOOP_SPAN, empty)).toEqual({});
  });
});

describe("disabled and inert", () => {
  it("runs callbacks with a no-op span when no agent host is configured", async () => {
    init({ service: "svc" });
    expect(tracer.trace("op", (span) => { span.setTag("a", "b").setMetric("m", 1).setResource("r").setError(new Error("x")); span.finish(); return span === NOOP_SPAN; })).toBe(true);
    expect(await tracer.trace("op", async () => "async")).toBe("async");
    expect(tracer.startSpan("m")).toBe(NOOP_SPAN);
    expect(tracer.scope().active()).toBeNull();
    expect(tracer.inject(undefined, {})).toEqual({});
    expect(tracer.stats()).toMatchObject({ spans: 0, chunksSent: 0, queued: 0 });
    expect(NOOP_SPAN.context().traceId).toBe("0".repeat(32));
    expect(NOOP_SPAN.traceId).toBe("0".repeat(32));
    expect(NOOP_SPAN.spanId).toBe("0".repeat(16));
    await tracer.flush();
    expect(agent.bodies).toHaveLength(0);
  });

  it("OZY_TRACE_ENABLED=false disables tracing but not the host's code", async () => {
    process.env["OZY_TRACE_ENABLED"] = "false";
    initTracing(agent);
    expect(tracer.trace("op", () => 5)).toBe(5);
    await tracer.flush();
    expect(agent.bodies).toHaveLength(0);
  });

  it("re-init that disables the SDK tears the tracer down", () => {
    expect(globalState().trace).not.toBeNull();
    init({});
    expect(globalState().trace).toBeNull();
  });

  it("a callback's error is unchanged when tracing is off", () => {
    init({});
    const e = new Error("mine");
    expect(() => tracer.trace("x", () => { throw e; })).toThrow(e);
  });
});

describe("stats", () => {
  it("counts spans and delivery", async () => {
    tracer.trace("a", {}, () => tracer.trace("b", {}, () => {}));
    await tracer.flush();
    expect(tracer.stats()).toMatchObject({ spans: 2, chunksQueued: 1, chunksSent: 1, chunksDropped: 0, queued: 0 });
  });
});

describe("shared state", () => {
  it("upgrades, in place, a state object an older copy of the package created", () => {
    const holder = globalThis as unknown as Record<symbol, Record<string, unknown>>;
    const key = Symbol.for("ozy");
    const current = holder[key]!;
    const old = { version: 1, statsd: null, exitHooksInstalled: true };
    holder[key] = old;
    try {
      const s = globalState();
      expect(s).toBe(old);
      expect(s.version).toBeGreaterThanOrEqual(2);
      expect(s.trace).toBeNull();
      expect(s.integrations).toBeInstanceOf(Map);
      expect(s.fetchAllow).toEqual([]);
      expect(s.exitHooksInstalled).toBe(true);
    } finally {
      holder[key] = current;
    }
  });
});
