// L10 safety suite (statsd parts): the SDK must be inert when unconfigured,
// must never throw into the host, must not swallow the host's own errors, and
// must share one state across duplicate module instances.
import * as dgram from "node:dgram";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { init, statsd } from "../src/index.js";
import { globalState } from "../src/state.js";
import { FakeAgent, clearEnv, closedPort, eventually, resetSdk } from "./helpers.js";

// Wrap the real dgram.createSocket in a spy so tests can prove no socket is
// ever created while the SDK is disabled, without replacing its behaviour.
vi.mock("node:dgram", async (importOriginal) => {
  const real = await importOriginal<typeof import("node:dgram")>();
  return { ...real, createSocket: vi.fn(real.createSocket) };
});

const exerciseEverything = async () => {
  statsd.increment("a");
  statsd.increment("a", 2, { tags: ["t:1"], sampleRate: 0.5 });
  statsd.decrement("b");
  statsd.gauge("c", 1);
  statsd.histogram("d", 1);
  statsd.distribution("e", 1);
  statsd.timing("f", 1);
  statsd.set("g", "x");
  expect(statsd.timed("h", () => 42)).toBe(42);
  expect(await statsd.timed("i", async () => "ok")).toBe("ok");
  statsd.flush();
  await statsd.close();
};

describe("safety", () => {
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
    vi.mocked(dgram.createSocket).mockClear();
  });
  afterEach(async () => {
    vi.restoreAllMocks();
    await resetSdk();
    restoreEnv();
  });

  describe("OZY_AGENT_HOST unset", () => {
    it("creates no socket, timer, or process hook — before or after init()", async () => {
      const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
      const setIntervalSpy = vi.spyOn(globalThis, "setInterval");
      const exitListeners = process.listenerCount("exit");
      const beforeExitListeners = process.listenerCount("beforeExit");
      const resources = process.getActiveResourcesInfo().sort();

      await exerciseEverything(); // before init()
      init({ service: "svc", env: "dev", tags: ["a:b"] });
      await exerciseEverything(); // after init() without a host
      process.env.OZY_AGENT_HOST = "";
      init();
      await exerciseEverything(); // empty host counts as unset

      expect(globalState().statsd).toBeNull();
      expect(dgram.createSocket).not.toHaveBeenCalled();
      expect(setTimeoutSpy).not.toHaveBeenCalled();
      expect(setIntervalSpy).not.toHaveBeenCalled();
      expect(process.listenerCount("exit")).toBe(exitListeners);
      expect(process.listenerCount("beforeExit")).toBe(beforeExitListeners);
      expect(process.getActiveResourcesInfo().sort()).toEqual(resources);
      expect(statsd.stats()).toEqual({ sent: 0, packets: 0, dropped: 0, errors: 0 });
    });

    it("an explicit empty agentHost opts out, even when the environment sets one", async () => {
      // How an app on an instrumented host turns metrics off for itself. The
      // argument used to fall through to the environment, so this silently
      // *enabled* the SDK and put the metric on the wire.
      process.env.OZY_AGENT_HOST = "127.0.0.1";
      process.env.OZY_STATSD_PORT = String(agent.port);
      init({ agentHost: "", service: "optout" });

      expect(globalState().statsd).toBeNull();
      statsd.increment("should.not.be.sent");
      statsd.flush();
      await new Promise((r) => setTimeout(r, 150));
      expect(agent.datagrams).toEqual([]);
    });

    it("prints one line in debug mode and nothing else", () => {
      const write = vi.spyOn(process.stderr, "write").mockImplementation(() => true);
      init({ debug: true });
      expect(write).toHaveBeenCalledWith(expect.stringContaining("disabled"));
    });
  });

  describe("enabled", () => {
    it("reads the agent address from the environment and creates the socket lazily", async () => {
      process.env.OZY_AGENT_HOST = "127.0.0.1";
      process.env.OZY_STATSD_PORT = String(agent.port);
      process.env.OZY_SERVICE = "env-svc";
      init();
      expect(dgram.createSocket).not.toHaveBeenCalled();
      statsd.increment("from.env");
      statsd.flush();
      expect(await agent.count(1)).toEqual(["from.env:1|c|#service:env-svc"]);
      expect(dgram.createSocket).toHaveBeenCalledTimes(1);
    });

    it("installs the exit hooks once, and they flush the buffer", async () => {
      const before = process.listeners("beforeExit").length;
      init({ agentHost: "127.0.0.1", statsdPort: agent.port, flushIntervalMs: 60_000 });
      init({ agentHost: "127.0.0.1", statsdPort: agent.port, flushIntervalMs: 60_000 });
      const hooks = process.listeners("beforeExit");
      expect(hooks.length).toBeLessThanOrEqual(before + 1);
      expect(globalState().exitHooksInstalled).toBe(true);
      statsd.increment("at.exit");
      const hook = hooks[hooks.length - 1] as () => void;
      hook();
      expect(await agent.count(1)).toEqual(["at.exit:1|c"]);
    });
  });

  describe("never throws into the host", () => {
    it("survives a closed port and counts the errors", async () => {
      init({ agentHost: "127.0.0.1", statsdPort: await closedPort() });
      for (let i = 0; i < 5; i++) {
        expect(() => {
          statsd.increment("x");
          statsd.flush();
        }).not.toThrow();
        await new Promise((r) => setTimeout(r, 10));
      }
      // The kernel reports ICMP port-unreachable on the connected socket.
      await eventually(() => statsd.stats().errors > 0);
    });

    it("survives a DNS failure and counts it", async () => {
      init({ agentHost: "nonexistent.invalid", statsdPort: 8125 });
      expect(() => {
        statsd.increment("x");
        statsd.flush();
      }).not.toThrow();
      await eventually(() => statsd.stats().errors === 1, 4000);
      expect(statsd.stats()).toMatchObject({ sent: 0, dropped: 1, errors: 1 });
    });

    it("survives hostile arguments", () => {
      init({ agentHost: "127.0.0.1", statsdPort: agent.port });
      const bad = null as unknown as string;
      const hostile = {
        toString() {
          throw new Error("hostile");
        },
      } as unknown as string;
      expect(() => statsd.increment(bad)).not.toThrow();
      expect(() => statsd.set("s", hostile)).not.toThrow();
      expect(() => statsd.gauge("g", 1, { tags: [hostile] })).not.toThrow();
      expect(statsd.stats().errors).toBe(2);
      // A bare string where an array belongs is the one hostile argument that
      // is *recoverable*: it is unambiguously one tag, so the metric is kept
      // rather than turned into a swallowed error.
      expect(() => statsd.gauge("g", 1, { tags: "one:tag" as unknown as string[] })).not.toThrow();
      expect(statsd.stats().errors).toBe(2);
      expect(() => init(null as unknown as undefined)).not.toThrow();
    });

    it("init() cannot throw even if resolving the config does", () => {
      const options = {
        get agentHost(): string {
          throw new Error("getter");
        },
      };
      expect(() => init(options)).not.toThrow();
    });

    it("flush/close/stats survive a broken client in the shared state", async () => {
      const boom = () => {
        throw new Error("broken");
      };
      globalState().statsd = { flush: boom, close: boom, stats: boom, metric: boom, set: boom, clock: boom, recordError: boom } as never;
      expect(() => statsd.flush()).not.toThrow();
      expect(() => statsd.increment("x")).not.toThrow();
      await expect(statsd.close()).resolves.toBeUndefined();
      expect(statsd.stats()).toEqual({ sent: 0, packets: 0, dropped: 0, errors: 0 });
      expect(statsd.timed("t", () => 7)).toBe(7);
      globalState().statsd = null;
    });
  });

  describe("timed()", () => {
    beforeEach(() => {
      let now = 0;
      init({
        agentHost: "127.0.0.1",
        statsdPort: agent.port,
        hooks: {
          now: () => {
            now += 25;
            return now;
          },
        },
      });
    });

    it("returns the sync result and records the duration", async () => {
      expect(statsd.timed("sync.ok", () => "v", { tags: ["k:v"] })).toBe("v");
      statsd.flush();
      expect(await agent.count(1)).toEqual(["sync.ok:25|ms|#k:v"]);
    });

    it("rethrows a sync error unchanged and still records", async () => {
      const err = new TypeError("user bug");
      let caught: unknown;
      try {
        statsd.timed("sync.err", () => {
          throw err;
        });
      } catch (e) {
        caught = e;
      }
      expect(caught).toBe(err);
      statsd.flush();
      expect(await agent.count(1)).toEqual(["sync.err:25|ms"]);
    });

    it("resolves with the async result and records when it settles", async () => {
      const result = statsd.timed("async.ok", async () => {
        await new Promise((r) => setTimeout(r, 5));
        return 99;
      });
      expect(result).toBeInstanceOf(Promise);
      expect(await result).toBe(99);
      statsd.flush();
      expect(await agent.count(1)).toEqual(["async.ok:25|ms"]);
    });

    it("rejects with the async error unchanged and still records", async () => {
      const err = new Error("async user bug");
      await expect(statsd.timed("async.err", () => Promise.reject(err))).rejects.toBe(err);
      statsd.flush();
      expect(await agent.count(1)).toEqual(["async.err:25|ms"]);
    });

    it("treats a value with a throwing `then` getter as synchronous", () => {
      const weird = Object.defineProperty({}, "then", {
        get() {
          throw new Error("getter");
        },
      });
      expect(statsd.timed("weird", () => weird)).toBe(weird);
    });

    it("runs the function when the SDK is disabled", async () => {
      await resetSdk();
      const fn = vi.fn(() => 1);
      expect(statsd.timed("off", fn)).toBe(1);
      expect(fn).toHaveBeenCalledOnce();
    });
  });

  describe("double module load (Next.js, dual ESM/CJS)", () => {
    it("two module instances share one client via globalThis", async () => {
      vi.resetModules();
      const a = await import("../src/index.js");
      vi.resetModules();
      const b = await import("../src/index.js");
      expect(a).not.toBe(b);
      expect(a.statsd).not.toBe(b.statsd);

      a.init({ agentHost: "127.0.0.1", statsdPort: agent.port, service: "shared" });
      b.statsd.increment("from.b");
      a.statsd.increment("from.a");
      b.statsd.flush();
      expect(await agent.count(1)).toEqual(["from.b:1|c|#service:shared\nfrom.a:1|c|#service:shared"]);
      expect(a.statsd.stats()).toEqual(b.statsd.stats());
      expect(globalState().statsd).not.toBeNull();
    });
  });
});
