// Buffering and flushing, observed on a real UDP socket.
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { type InitOptions, init, statsd } from "../src/index.js";
import type { TimerFns } from "../src/transport.js";
import { FakeAgent, clearEnv, resetSdk } from "./helpers.js";

describe("buffering", () => {
  let agent: FakeAgent;
  let restoreEnv: () => void;

  beforeAll(async () => {
    agent = await FakeAgent.start();
  });
  afterAll(async () => {
    await agent.close();
  });
  beforeEach(() => {
    restoreEnv = clearEnv();
    agent.clear();
  });
  afterEach(async () => {
    await resetSdk();
    restoreEnv();
  });

  const start = (extra: InitOptions = {}) => init({ agentHost: "127.0.0.1", statsdPort: agent.port, ...extra });

  it("coalesces several messages into one datagram joined by newlines", async () => {
    start();
    statsd.increment("a");
    statsd.gauge("b", 2);
    statsd.set("c", "x");
    statsd.flush();
    expect(await agent.count(1)).toEqual(["a:1|c\nb:2|g\nc:x|s"]);
    expect(statsd.stats()).toEqual({ sent: 3, packets: 1, dropped: 0, errors: 0 });
  });

  it("splits at maxPayloadBytes without exceeding it", async () => {
    // Each line "m.NN:1|c" is 8 bytes; 3 lines + 2 newlines = 26 ≤ 30, a 4th would be 35.
    start({ maxPayloadBytes: 30 });
    for (let i = 10; i < 20; i++) statsd.increment(`m.${i}`);
    statsd.flush();
    const got = await agent.count(4);
    expect(got).toEqual([
      "m.10:1|c\nm.11:1|c\nm.12:1|c",
      "m.13:1|c\nm.14:1|c\nm.15:1|c",
      "m.16:1|c\nm.17:1|c\nm.18:1|c",
      "m.19:1|c",
    ]);
    for (const d of got) expect(Buffer.byteLength(d)).toBeLessThanOrEqual(30);
  });

  it("sends a buffer that exactly reaches the limit immediately", async () => {
    start({ maxPayloadBytes: 8, flushIntervalMs: 60_000 });
    statsd.increment("m.10");
    expect(await agent.count(1)).toEqual(["m.10:1|c"]);
  });

  it("sends a single oversized line alone rather than truncating it", async () => {
    start({ maxPayloadBytes: 16 });
    statsd.increment("short");
    statsd.increment("a.very.long.metric.name");
    statsd.flush();
    expect(await agent.count(2)).toEqual(["short:1|c", "a.very.long.metric.name:1|c"]);
  });

  it("flushes on the timer without an explicit flush()", async () => {
    start({ flushIntervalMs: 20 });
    const t0 = Date.now();
    statsd.increment("timer.flush");
    expect(await agent.count(1)).toEqual(["timer.flush:1|c"]);
    expect(Date.now() - t0).toBeGreaterThanOrEqual(15);
  });

  it("arms one unref'd timer per non-empty buffer and clears it on flush", async () => {
    const unref = vi.fn();
    const handles: object[] = [];
    const timers: TimerFns = {
      set: vi.fn(() => {
        const h = { unref };
        handles.push(h);
        return h;
      }),
      clear: vi.fn(),
    };
    start({ hooks: { timers } });
    statsd.increment("a");
    statsd.increment("b");
    expect(timers.set).toHaveBeenCalledTimes(1);
    expect(timers.set).toHaveBeenCalledWith(expect.any(Function), 100);
    expect(unref).toHaveBeenCalledTimes(1);
    statsd.flush();
    expect(timers.clear).toHaveBeenCalledWith(handles[0]);
    statsd.flush(); // empty: nothing to do
    expect(timers.set).toHaveBeenCalledTimes(1);
    expect(await agent.count(1)).toEqual(["a:1|c\nb:1|c"]);
  });

  it("close() flushes, releases the socket, and ignores later calls", async () => {
    start();
    statsd.increment("before.close");
    await statsd.close();
    expect(await agent.count(1)).toEqual(["before.close:1|c"]);
    statsd.increment("after.close");
    statsd.flush();
    await statsd.close(); // idempotent
    await new Promise((r) => setTimeout(r, 30));
    expect(agent.datagrams).toEqual(["before.close:1|c"]);
    expect(statsd.stats().sent).toBe(1);
  });

  it("re-init replaces the client and flushes the old buffer", async () => {
    start({ service: "one" });
    statsd.increment("x");
    start({ service: "two" });
    statsd.increment("y");
    statsd.flush();
    const got = await agent.count(2);
    expect(got.sort()).toEqual(["x:1|c|#service:one", "y:1|c|#service:two"]);
  });

  it("supports the increment(name, opts) shorthand and decrement defaults", async () => {
    start();
    statsd.increment("a", { tags: ["t:1"] });
    statsd.decrement("b", { sampleRate: 1 });
    statsd.decrement("c");
    statsd.flush();
    expect(await agent.count(1)).toEqual(["a:1|c|#t:1\nb:-1|c\nc:-1|c"]);
  });

  it("drops NaN/Inf values and counts them", () => {
    start();
    statsd.gauge("g", NaN);
    statsd.timing("t", Infinity);
    expect(statsd.stats().dropped).toBe(2);
  });

  it("never sends with a NaN, zero or negative sample rate", async () => {
    start({ hooks: { random: () => 0 } });
    statsd.increment("nan", 1, { sampleRate: NaN });
    statsd.increment("zero", 1, { sampleRate: 0 });
    statsd.increment("neg", 1, { sampleRate: -1 });
    statsd.increment("big", 1, { sampleRate: 5 });
    statsd.flush();
    expect(await agent.count(1)).toEqual(["big:1|c"]);
  });

  it("logs to the debug sink when debug is on", async () => {
    const log = vi.fn();
    start({ debug: true, hooks: { log } });
    statsd.gauge("bad", NaN);
    statsd.increment("ok");
    statsd.flush();
    await agent.count(1);
    expect(log).toHaveBeenCalledWith(expect.stringContaining("enabled: agent 127.0.0.1"));
    expect(log).toHaveBeenCalledWith(expect.stringContaining("not a finite number"));
  });
});
