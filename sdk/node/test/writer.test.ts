// The trace writer: bounds, drop-oldest, no retries, timeouts, and that it can
// neither throw into the host nor keep the process alive.
import { execFileSync, spawn } from "node:child_process";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { pathToFileURL } from "node:url";
import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { FLUSH_AT_CHUNKS, MAX_QUEUE_CHUNKS, TraceWriter } from "../src/trace/writer.js";
import type { WireSpan } from "../src/trace/wire.js";
import { closedPort, eventually } from "./helpers.js";
import { FakeTraceAgent, type SeenSpan } from "./trace-helpers.js";

let agent: FakeTraceAgent;
beforeAll(async () => {
  agent = await FakeTraceAgent.start();
});
afterAll(async () => {
  await agent.close();
});
afterEach(() => {
  agent.clear();
  agent.hang = false;
  agent.status = 200;
  agent.rates = {};
});

const info = { lang: "node", lang_version: "22", version: "0" };
function chunk(tag: string, n = 1): WireSpan[] {
  return Array.from({ length: n }, (_v, i) => ({
    trace_id: "1".repeat(32),
    span_id: String(i).padStart(16, "0"),
    parent_id: null,
    service: "s",
    name: tag,
    resource: tag,
    type: "custom",
    start: 1_700_000_000_000_000,
    duration: 1,
    error: 0,
    meta: {},
    metrics: {},
  }));
}
const names = (cs: SeenSpan[][]) => cs.map((c) => c[0]!.name);

describe("TraceWriter", () => {
  it("posts the §B body: tracer info plus chunks", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    w.enqueue(chunk("a", 2));
    await w.flush();
    expect(agent.bodies).toHaveLength(1);
    expect(agent.bodies[0]!.tracer).toEqual(info);
    expect(agent.bodies[0]!.traces).toHaveLength(1);
    expect(agent.bodies[0]!.traces[0]).toHaveLength(2);
    expect(w.stats()).toMatchObject({ chunksSent: 1, chunksDropped: 0, queued: 0 });
  });

  it("flushes on its own after the interval, and not before", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, flushIntervalMs: 40 });
    w.enqueue(chunk("a"));
    expect(agent.bodies).toHaveLength(0);
    await new Promise((r) => setTimeout(r, 250));
    expect(agent.bodies).toHaveLength(1);
    await w.close();
  });

  it("flushes early at 100 queued chunks", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, flushIntervalMs: 60_000 });
    for (let i = 0; i < FLUSH_AT_CHUNKS; i++) w.enqueue(chunk(`c${i}`));
    // No explicit flush: the 100th chunk must trigger it, long before the 60 s timer.
    await eventually(() => agent.chunks().length === FLUSH_AT_CHUNKS);
  });

  it("99 chunks wait for the timer (the early flush is exactly at 100)", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, flushIntervalMs: 60_000 });
    for (let i = 0; i < FLUSH_AT_CHUNKS - 1; i++) w.enqueue(chunk(`c${i}`));
    await new Promise((r) => setTimeout(r, 50));
    expect(agent.chunks()).toHaveLength(0);
  });

  it("the flush timer is unref'd, so an idle queue cannot keep the process alive", () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, flushIntervalMs: 60_000 });
    w.enqueue(chunk("a"));
    const timer = (w as unknown as { timer: NodeJS.Timeout }).timer;
    expect(timer.hasRef()).toBe(false);
    clearTimeout(timer);
  });

  it("clamps rates from the agent into [0, 1] before handing them on", async () => {
    let got: Record<string, number> = {};
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, onRates: (r) => (got = r) });
    agent.rates = { hi: 7, lo: -2, ok: 0.3 };
    w.enqueue(chunk("a"));
    await w.flush();
    expect(got).toEqual({ hi: 1, lo: 0, ok: 0.3 });
  });

  it("drops the OLDEST chunk past 1000 and counts it", async () => {
    const big = new TraceWriter({ host: "127.0.0.1", port: await closedPort(), info, flushIntervalMs: 60_000 });
    // Hold an in-flight flush so enqueue()'s early flush returns the same promise instead of draining.
    (big as unknown as { inflight: Promise<void> }).inflight = new Promise(() => {});
    for (let i = 0; i < MAX_QUEUE_CHUNKS + 25; i++) big.enqueue(chunk(`c${i}`));
    const s = big.stats();
    expect(s.queued).toBe(MAX_QUEUE_CHUNKS);
    expect(s.chunksDropped).toBe(25);
    expect(s.chunksQueued).toBe(MAX_QUEUE_CHUNKS + 25);
    const q = (big as unknown as { queue: Array<{ body: string }> }).queue;
    expect(JSON.parse(q[0]!.body)[0].name).toBe("c25");
    expect(JSON.parse(q[q.length - 1]!.body)[0].name).toBe(`c${MAX_QUEUE_CHUNKS + 24}`);
  });

  it("splits a full queue into requests the agent accepts (<= 1000 chunks, bounded bytes)", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, flushIntervalMs: 60_000 });
    // 20 chunks of ~1 MiB each: 8 MiB cap means several requests.
    const fat = (i: number) => chunk(`f${i}`).map((s) => ({ ...s, resource: "x".repeat(900_000) }));
    (w as unknown as { inflight: Promise<void> }).inflight = new Promise(() => {});
    for (let i = 0; i < 20; i++) w.enqueue(fat(i));
    (w as unknown as { inflight: null }).inflight = null;
    await w.flush();
    expect(agent.bodies.length).toBeGreaterThan(1);
    for (const raw of agent.raw) expect(Buffer.byteLength(raw)).toBeLessThanOrEqual(8 * 1024 * 1024 + 1024);
    expect(agent.chunks()).toHaveLength(20);
  });

  it("drops a single chunk that alone exceeds the body budget", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    w.enqueue(chunk("huge").map((s) => ({ ...s, meta: { a: "y".repeat(9 * 1024 * 1024) } })));
    await w.flush();
    expect(agent.bodies).toHaveLength(0);
    expect(w.stats().chunksDropped).toBe(1);
  });

  it("caps a chunk at the agent's 5000-span limit", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    w.enqueue(chunk("many", 5200));
    await w.flush();
    expect(agent.chunks()[0]).toHaveLength(5000);
  });

  it("ignores an empty chunk", () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    w.enqueue([]);
    expect(w.stats().chunksQueued).toBe(0);
  });

  it("does not retry: a 429 or 500 drops the chunks and the next flush sends only new ones", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    agent.status = 429;
    w.enqueue(chunk("lost"));
    await w.flush();
    agent.status = 200;
    w.enqueue(chunk("kept"));
    await w.flush();
    expect(agent.bodies).toHaveLength(2);
    expect(names(agent.bodies[1]!.traces)).toEqual(["kept"]);
    expect(w.stats()).toMatchObject({ chunksSent: 1, chunksDropped: 1 });
  });

  it("survives a refused connection: counts the error, never throws, never rejects", async () => {
    const logs: string[] = [];
    const w = new TraceWriter({ host: "127.0.0.1", port: await closedPort(), info, log: (m) => logs.push(m) });
    w.enqueue(chunk("a"));
    await expect(w.flush()).resolves.toBeUndefined();
    expect(w.stats()).toMatchObject({ chunksDropped: 1, errors: 1 });
    expect(logs.join()).toContain("trace request failed");
  });

  it("gives up on a hung agent after the request timeout", async () => {
    agent.hang = true;
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, requestTimeoutMs: 80 });
    w.enqueue(chunk("a"));
    const t0 = Date.now();
    await w.flush();
    expect(Date.now() - t0).toBeLessThan(1500);
    expect(w.stats()).toMatchObject({ chunksDropped: 1, errors: 1 });
  });

  it("treats a 200 with an unreadable body as delivered, and ignores a throwing onRates", async () => {
    const w = new TraceWriter({
      host: "127.0.0.1",
      port: agent.port,
      info,
      onRates: () => {
        throw new Error("cb");
      },
    });
    agent.rates = { "service:a,env:b": 0.5 };
    w.enqueue(chunk("a"));
    await w.flush();
    expect(w.stats().chunksSent).toBe(1);
  });

  it("flushOnExit returns within its budget even if the agent hangs", async () => {
    agent.hang = true;
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info, requestTimeoutMs: 5000 });
    w.enqueue(chunk("a"));
    const t0 = Date.now();
    await w.flushOnExit(150);
    expect(Date.now() - t0).toBeLessThan(1000);
    await w.close().catch(() => {});
  });

  it("refuses chunks after close()", async () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    await w.close();
    w.enqueue(chunk("late"));
    expect(w.stats().chunksQueued).toBe(0);
  });

  it("an unserializable chunk is counted, not thrown", () => {
    const w = new TraceWriter({ host: "127.0.0.1", port: agent.port, info });
    const circular = chunk("c");
    (circular[0] as unknown as { self: unknown }).self = circular[0];
    expect(() => w.enqueue(circular)).not.toThrow();
    expect(w.stats().errors).toBe(1);
  });

  describe("in a real child process", () => {
    let dist: string;
    beforeAll(() => {
      // Compile the SDK once; a child cannot import .ts directly because the sources use .js specifiers.
      dist = mkdtempSync(join(tmpdir(), "ozy-dist-"));
      execFileSync(process.execPath, ["node_modules/typescript/bin/tsc", "-p", "tsconfig.esm.json", "--outDir", dist], { stdio: "pipe" });
      writeFileSync(join(dist, "package.json"), '{"type":"module"}');
    }, 60_000);
    afterAll(() => rmSync(dist, { recursive: true, force: true }));

    const run = (port: number) =>
      new Promise<number>((resolve, reject) => {
        const code = `
          import { init, tracer } from ${JSON.stringify(pathToFileURL(join(dist, "index.js")).href)};
          init({ service: "child", agentHost: "127.0.0.1", tracePort: ${port} });
          tracer.trace("op", () => {});
        `;
        const t0 = Date.now();
        const child = spawn(process.execPath, ["--input-type=module", "-e", code], { stdio: "ignore" });
        const killer = setTimeout(() => {
          child.kill("SIGKILL");
          reject(new Error("child did not exit: the tracer kept the process alive"));
        }, 10_000);
        child.on("exit", () => {
          clearTimeout(killer);
          resolve(Date.now() - t0);
        });
      });

    it("exits on its own and delivers the last trace through the exit flush", async () => {
      await run(agent.port);
      expect(agent.spans().map((s) => s.name)).toEqual(["op"]);
    });

    it("exits within the 1 s exit budget even when the agent never answers", async () => {
      agent.hang = true;
      const ms = await run(agent.port);
      expect(ms).toBeLessThan(5000);
    });

    it("exits promptly when nothing is listening", async () => {
      const ms = await run(await closedPort());
      expect(ms).toBeLessThan(5000);
    });
  });
});
