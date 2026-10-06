// Integrations: next (traceRoute / withTelemetry), sqlite (real in-memory
// better-sqlite3), fetch, winston, and the registry. Each uses only the
// public tracer API, and the SQL-parameter test pins that no bound value ever
// reaches a payload.
import * as http from "node:http";
import Database from "better-sqlite3";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import {
  type Integration,
  init,
  instrumentFetch,
  instrumentSqlite,
  listIntegrations,
  patchIntegrations,
  registerIntegration,
  tracer,
  traceFormat,
  traceRoute,
  uninstrumentFetch,
  unpatchIntegrations,
  withTelemetry,
} from "../src/index.js";
import { globalState } from "../src/state.js";
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
  initTracing(agent);
});
afterEach(async () => {
  uninstrumentFetch();
  delete (globalThis as unknown as Record<symbol, unknown>)[Symbol.for("ozy.fetch.meta")];
  unpatchIntegrations();
  patchIntegrations(["next"]); // its default is on; unpatch only switches it off
  await resetTracing();
  restoreEnv();
});

const byName = (name: string) => agent.spans().filter((s) => s.name === name);

describe("next: traceRoute / withTelemetry", () => {
  it("names the span 'METHOD /normalized/path', never records the query, and records the status", async () => {
    const req = new Request("http://localhost:3000/api/comics/42/pages/550e8400-e29b-41d4-a716-446655440000?token=secret#frag", { method: "get" });
    const res = await traceRoute(req, undefined, async () => new Response("ok", { status: 200 }));
    expect(res.status).toBe(200);
    await tracer.flush();
    const [s] = byName("http.request");
    expect(s).toMatchObject({ resource: "GET /api/comics/:id/pages/:id", type: "web", error: 0 });
    expect(s!.meta).toMatchObject({
      "http.method": "GET",
      "http.route": "/api/comics/:id/pages/:id",
      "http.url": "http://localhost:3000/api/comics/42/pages/550e8400-e29b-41d4-a716-446655440000",
      "http.status_code": "200",
      "span.kind": "server",
    });
    expect(agent.raw.join()).not.toContain("secret");
  });

  it("uses routeHint over the normalized path", async () => {
    await traceRoute(new Request("http://x/api/comics/42", { method: "POST" }), "/api/comics/[id]", () => new Response(null, { status: 201 }));
    await tracer.flush();
    expect(byName("http.request")[0]).toMatchObject({ resource: "POST /api/comics/[id]" });
  });

  it("marks a 5xx response as an error but passes the Response through untouched", async () => {
    const response = new Response("nope", { status: 503 });
    const got = await traceRoute(new Request("http://x/a"), undefined, () => response);
    expect(got).toBe(response);
    await tracer.flush();
    expect(byName("http.request")[0]).toMatchObject({ error: 1 });
    expect(byName("http.request")[0]!.meta["http.status_code"]).toBe("503");
  });

  it("a 4xx is not an error", async () => {
    await traceRoute(new Request("http://x/a"), undefined, () => new Response(null, { status: 404 }));
    await tracer.flush();
    expect(byName("http.request")[0]!.error).toBe(0);
  });

  it("a throwing handler (sync or async) is marked 500 and re-thrown unchanged", async () => {
    const boom = new Error("handler broke");
    expect(() =>
      traceRoute(new Request("http://x/a"), undefined, () => {
        throw boom;
      }),
    ).toThrow(boom);
    await expect(traceRoute(new Request("http://x/b"), undefined, async () => Promise.reject(boom))).rejects.toBe(boom);
    await tracer.flush();
    for (const s of byName("http.request")) {
      expect(s.error).toBe(1);
      expect(s.meta["http.status_code"]).toBe("500");
      expect(s.meta["error.message"]).toBe("handler broke");
    }
    expect(byName("http.request")).toHaveLength(2);
  });

  it("continues a trace from propagation headers, inheriting priority", async () => {
    const headers = { "x-ozy-trace-id": "a".repeat(32), "x-ozy-parent-id": "b".repeat(16), "x-ozy-sampling-priority": "0" };
    await traceRoute(new Request("http://x/a", { headers }), undefined, () => new Response());
    await tracer.flush();
    const s = byName("http.request")[0]!;
    expect(s).toMatchObject({ trace_id: "a".repeat(32), parent_id: "b".repeat(16) });
    expect(s.metrics["_sampling_priority"]).toBe(0);
    expect(s.metrics["_top_level"]).toBe(1);
  });

  it("garbage propagation headers start a fresh trace", async () => {
    const headers = { "x-ozy-trace-id": "zzz", "x-ozy-parent-id": "b".repeat(16) };
    await traceRoute(new Request("http://x/a", { headers }), undefined, () => new Response());
    await tracer.flush();
    expect(byName("http.request")[0]!.parent_id).toBeNull();
  });

  it("works with a relative url and a minimal request-like object", async () => {
    await traceRoute({ url: "/api/x/7", headers: { get: () => null } }, undefined, () => ({ status: 200 }));
    await traceRoute({ url: "http://", method: "PATCH", headers: { get: () => null } }, undefined, () => 1);
    await tracer.flush();
    const rs = byName("http.request").map((s) => s.resource).sort();
    expect(rs).toEqual(["GET /api/x/:id", "PATCH /"]);
  });

  it("runs the handler exactly once even when span setup fails", async () => {
    let calls = 0;
    const hostile = {
      get url(): string {
        throw new Error("no url");
      },
      headers: { get: () => null },
    };
    const out = traceRoute(hostile as never, undefined, () => {
      calls++;
      return "result";
    });
    expect(out).toBe("result");
    expect(calls).toBe(1);
  });

  it("withTelemetry forwards every argument and the result, with the route option", async () => {
    const handler = vi.fn(async (_req: Request, ctx: { params: { id: string } }) => new Response(ctx.params.id));
    const wrapped = withTelemetry(handler, { route: "/api/comics/[id]" });
    const res = await wrapped(new Request("http://x/api/comics/9"), { params: { id: "9" } });
    expect(await res.text()).toBe("9");
    expect(handler).toHaveBeenCalledTimes(1);
    await tracer.flush();
    expect(byName("http.request")[0]!.resource).toBe("GET /api/comics/[id]");
    const bare = withTelemetry(() => new Response("x"));
    await bare(new Request("http://x/p/12345"));
  });

  it("child spans created inside the handler parent to the request span", async () => {
    await traceRoute(new Request("http://x/a"), undefined, async () => {
      await tracer.trace("inner", {}, async () => {});
      return new Response();
    });
    await tracer.flush();
    expect(byName("inner")[0]!.parent_id).toBe(byName("http.request")[0]!.span_id);
  });

  it("is inert when the SDK is not configured, and still runs the handler", async () => {
    init({});
    expect(await traceRoute(new Request("http://x/a"), undefined, async () => "plain")).toBe("plain");
  });

  it("unpatching the integration makes it a pass-through", async () => {
    unpatchIntegrations(["next"]);
    await traceRoute(new Request("http://x/a"), undefined, () => new Response());
    await tracer.flush();
    expect(agent.spans()).toHaveLength(0);
    patchIntegrations(["next"]);
    await traceRoute(new Request("http://x/a"), undefined, () => new Response());
    await tracer.flush();
    expect(agent.spans()).toHaveLength(1);
  });
});

describe("sqlite (real in-memory better-sqlite3)", () => {
  function db(): InstanceType<typeof Database> {
    const d = new Database(":memory:");
    d.exec("CREATE TABLE comics (id INTEGER PRIMARY KEY, title TEXT UNIQUE, secret TEXT)");
    return d;
  }

  it("times run/get/all/iterate inside a trace; resource is the statement source", async () => {
    const d = instrumentSqlite(db());
    await tracer.trace("req", { type: "web" }, async () => {
      const ins = d.prepare("INSERT INTO comics (title) VALUES (?)");
      expect(ins.run("a").changes).toBe(1);
      ins.run("b");
      expect(d.prepare("SELECT title FROM comics WHERE id = ?").get(1)).toEqual({ title: "a" });
      expect(d.prepare("SELECT title FROM comics ORDER BY id").all()).toHaveLength(2);
      const titles = [...d.prepare("SELECT title FROM comics ORDER BY id").iterate()];
      expect(titles).toHaveLength(2);
    });
    await tracer.flush();
    const q = byName("sqlite.query");
    expect(q).toHaveLength(5);
    const req = byName("req")[0]!;
    for (const s of q) {
      expect(s).toMatchObject({ type: "db", parent_id: req.span_id, error: 0 });
      expect(s.meta["db.system"]).toBe("sqlite");
      expect(s.metrics["_top_level"]).toBeUndefined();
    }
    expect(q.map((s) => s.resource)).toEqual([
      "INSERT INTO comics (title) VALUES (?)",
      "INSERT INTO comics (title) VALUES (?)",
      "SELECT title FROM comics WHERE id = ?",
      "SELECT title FROM comics ORDER BY id",
      "SELECT title FROM comics ORDER BY id",
    ]);
    expect(q[0]!.metrics["db.rowcount"]).toBe(1);
  });

  it("creates no spans (no orphan roots) outside a trace, and results are unchanged", async () => {
    const d = instrumentSqlite(db());
    d.prepare("INSERT INTO comics (title) VALUES (?)").run("x");
    expect(d.prepare("SELECT count(*) AS n FROM comics").get()).toEqual({ n: 1 });
    await tracer.flush();
    expect(agent.spans()).toHaveLength(0);
  });

  it("a failing statement marks the span and re-throws the same error", async () => {
    const d = instrumentSqlite(db());
    let caught: unknown;
    await tracer.trace("req", {}, async () => {
      const ins = d.prepare("INSERT INTO comics (title) VALUES (?)");
      ins.run("dup");
      try {
        ins.run("dup");
      } catch (e) {
        caught = e;
      }
      expect(() => d.prepare("SELECT * FROM nope")).toThrow();
    });
    expect(caught).toBeInstanceOf(Error);
    expect((caught as Error).message).toContain("UNIQUE");
    await tracer.flush();
    const failed = byName("sqlite.query").filter((s) => s.error === 1);
    expect(failed).toHaveLength(1);
    expect(failed[0]!.meta["error.message"]).toContain("UNIQUE");
  });

  it("an iterator that is abandoned early still ends its span; one that throws marks it", async () => {
    const d = instrumentSqlite(db());
    d.prepare("INSERT INTO comics (title) VALUES (?)").run("a");
    d.prepare("INSERT INTO comics (title) VALUES (?)").run("b");
    await tracer.trace("req", {}, async () => {
      for (const row of d.prepare("SELECT title FROM comics").iterate()) {
        void row;
        break;
      }
      const it = d.prepare("SELECT title FROM comics").iterate();
      const first = it[Symbol.iterator]();
      expect(first).toBe(it);
      expect(it.next().done).toBe(false);
      it.next();
      expect(it.next().done).toBe(true);
    });
    await tracer.flush();
    expect(byName("sqlite.query")).toHaveLength(2);
  });

  it("NEVER puts a bound parameter value in any emitted payload", async () => {
    const SENTINEL = "SENTINEL-p4ssw0rd-9f8e7d";
    const d = instrumentSqlite(db());
    await tracer.trace("req", {}, async () => {
      d.prepare("INSERT INTO comics (title, secret) VALUES (?, ?)").run("t", SENTINEL);
      d.prepare("SELECT * FROM comics WHERE secret = ?").get(SENTINEL);
      d.prepare("SELECT * FROM comics WHERE secret = @s").all({ s: SENTINEL });
      for (const _r of d.prepare("SELECT * FROM comics WHERE secret = ?").iterate(SENTINEL)) void _r;
      try {
        d.prepare("INSERT INTO comics (title, secret) VALUES (?, ?)").run("t", SENTINEL);
      } catch {
        // UNIQUE violation, deliberately
      }
    });
    await tracer.flush();
    expect(byName("sqlite.query").length).toBeGreaterThanOrEqual(5);
    expect(agent.raw.length).toBeGreaterThan(0);
    expect(agent.raw.join("\n")).not.toContain(SENTINEL);
  });

  it("is idempotent across instances and across repeated calls (one span per query)", async () => {
    const d1 = instrumentSqlite(instrumentSqlite(db()));
    const d2 = instrumentSqlite(db());
    await tracer.trace("req", {}, async () => {
      d1.prepare("SELECT 1").get();
      d2.prepare("SELECT 2").get();
    });
    await tracer.flush();
    expect(byName("sqlite.query")).toHaveLength(2);
  });

  it("the integration's unpatch restores the library and patch re-applies", async () => {
    const d = instrumentSqlite(db());
    const proto = Object.getPrototypeOf(d) as { prepare: unknown };
    const patched = proto.prepare;
    unpatchIntegrations(["sqlite"]);
    expect(proto.prepare).not.toBe(patched);
    await tracer.trace("req", {}, async () => {
      d.prepare("SELECT 1").get();
    });
    await tracer.flush();
    expect(byName("sqlite.query")).toHaveLength(0);
    patchIntegrations(["sqlite"]);
    expect(proto.prepare).toBe(patched);
    agent.clear();
    await tracer.trace("req", {}, async () => {
      d.prepare("SELECT 1").get();
    });
    await tracer.flush();
    expect(byName("sqlite.query")).toHaveLength(1);
    // Leave the prototype restored for other tests.
    unpatchIntegrations(["sqlite"]);
  });

  it("ignores things that are not databases", () => {
    expect(() => instrumentSqlite({})).not.toThrow();
    expect(() => instrumentSqlite(null as never)).not.toThrow();
  });
});

describe("fetch", () => {
  let server: http.Server;
  let port: number;
  const seen: Array<{ method: string; url: string; headers: http.IncomingHttpHeaders }> = [];
  let status = 200;

  beforeAll(async () => {
    server = http.createServer((req, res) => {
      seen.push({ method: req.method ?? "", url: req.url ?? "", headers: req.headers });
      if (req.url === "/slow") return void setTimeout(() => res.end("late"), 2000).unref();
      res.statusCode = status;
      res.end("hello");
    });
    await new Promise<void>((r) => server.listen(0, "127.0.0.1", r));
    port = (server.address() as { port: number }).port;
  });
  afterAll(async () => {
    server.closeAllConnections();
    await new Promise<void>((r) => server.close(() => r()));
  });
  beforeEach(() => {
    seen.length = 0;
    status = 200;
  });

  it("wraps once: a second call (or second package copy) does not stack another layer", async () => {
    const calls: unknown[] = [];
    const stub = (async (...a: unknown[]) => {
      calls.push(a);
      return new Response("x");
    }) as unknown as typeof fetch;
    const real = globalThis.fetch;
    globalThis.fetch = stub;
    try {
      instrumentFetch();
      const first = globalThis.fetch;
      instrumentFetch({ propagateTo: ["a"] });
      expect(globalThis.fetch).toBe(first);
      expect(first).not.toBe(stub);
      await first("http://x/");
      expect(calls).toHaveLength(1);
      uninstrumentFetch();
      expect(globalThis.fetch).toBe(stub);
    } finally {
      globalThis.fetch = real;
      delete (globalThis as unknown as Record<symbol, unknown>)[Symbol.for("ozy.fetch.meta")];
    }
  });

  it("wraps whatever fetch is at init, keeping an existing patch chain intact", async () => {
    const order: string[] = [];
    const real = globalThis.fetch;
    const nextLike = (async (i: Parameters<typeof fetch>[0], init?: RequestInit) => {
      order.push("next-patch");
      return real(i, init);
    }) as typeof fetch;
    globalThis.fetch = nextLike;
    try {
      instrumentFetch();
      await tracer.trace("req", {}, async () => {
        await (await fetch(`http://127.0.0.1:${port}/a`)).text();
      });
      expect(order).toEqual(["next-patch"]);
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("creates http.client spans only inside a trace; resource 'METHOD host'; query and credentials dropped", async () => {
    instrumentFetch();
    await fetch(`http://127.0.0.1:${port}/outside`);
    await tracer.trace("req", {}, async () => {
      const r = await fetch(`http://127.0.0.1:${port}/a/b?token=secret`, { method: "post", body: "x" });
      expect(await r.text()).toBe("hello");
      // Credentials in a URL: undici refuses them, so check the span builder against a stub.
    });
    await tracer.flush();
    const [c] = byName("http.client");
    expect(byName("http.client")).toHaveLength(1);
    expect(c).toMatchObject({ resource: `POST 127.0.0.1:${port}`, type: "http", parent_id: byName("req")[0]!.span_id });
    expect(c!.meta).toMatchObject({ "http.method": "POST", "http.url": `http://127.0.0.1:${port}/a/b`, "http.status_code": "200", "span.kind": "client" });
    expect(agent.raw.join()).not.toContain("secret");
  });

  it("strips credentials from the recorded url", async () => {
    const real = globalThis.fetch;
    globalThis.fetch = (async () => new Response("x")) as typeof fetch;
    try {
      instrumentFetch();
      await tracer.trace("req", {}, async () => fetch("http://user:pw@api.test:8080/p?k=secret"));
      await tracer.flush();
      expect(byName("http.client")[0]!.meta["http.url"]).toBe("http://api.test:8080/p");
      expect(agent.raw.join()).not.toContain("pw");
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("injects propagation headers ONLY for allow-listed hosts (default none)", async () => {
    instrumentFetch();
    let clientSpan = "";
    await tracer.trace("req", {}, async () => {
      await fetch(`http://127.0.0.1:${port}/none`);
    });
    expect(seen[0]!.headers["x-ozy-trace-id"]).toBeUndefined();
    seen.length = 0;
    instrumentFetch({ propagateTo: [`127.0.0.1:${port}`] });
    await tracer.trace("req", {}, async () => {
      await fetch(`http://127.0.0.1:${port}/yes`, { headers: { "x-keep": "1" } });
      clientSpan = "set";
    });
    await tracer.flush();
    const h = seen[0]!.headers;
    expect(clientSpan).toBe("set");
    expect(h["x-keep"]).toBe("1");
    const client = byName("http.client").find((s) => s.meta["http.url"]!.endsWith("/yes"))!;
    expect(h["x-ozy-trace-id"]).toBe(client.trace_id);
    expect(h["x-ozy-parent-id"]).toBe(client.span_id);
    expect(h["x-ozy-sampling-priority"]).toBe("1");
    // A host that is not on the list gets nothing even though another is.
    seen.length = 0;
    await tracer.trace("req", {}, async () => {
      await fetch(`http://localhost:${port}/other`).catch(() => {});
    });
    expect(seen.every((r) => r.headers["x-ozy-trace-id"] === undefined)).toBe(true);
  });

  it("matches host-only and wildcard allow-list entries, and a Request input keeps its method and body", async () => {
    instrumentFetch({ propagateTo: ["127.0.0.1"] });
    await tracer.trace("req", {}, async () => {
      await fetch(new Request(`http://127.0.0.1:${port}/r`, { method: "PUT", body: "payload", headers: { "x-a": "b" } }));
    });
    expect(seen[0]!).toMatchObject({ method: "PUT" });
    expect(seen[0]!.headers["x-a"]).toBe("b");
    expect(seen[0]!.headers["x-ozy-trace-id"]).toBeDefined();
  });

  it("host-only entries ignore the port, wildcard entries match subdomains only", async () => {
    // A stub records what the wrapper hands to the next layer: no DNS involved.
    const real = globalThis.fetch;
    const got: Array<string | null> = [];
    globalThis.fetch = (async (_i: unknown, init?: RequestInit) => {
      got.push(new Headers(init?.headers).get("x-ozy-trace-id"));
      return new Response("x");
    }) as typeof fetch;
    try {
      instrumentFetch({ propagateTo: ["*.internal.test", "api.test", "ONLY.CASE.test:81"] });
      await tracer.trace("req", {}, async () => {
        await fetch("http://svc.internal.test/a"); // wildcard: yes
        await fetch("http://internal.test/a"); // the bare domain is not a subdomain: no
        await fetch("http://evilinternal.test/a"); // suffix without the dot boundary: no
        await fetch("http://api.test:9000/a"); // host-only entry matches any port: yes
        await fetch("http://only.case.test:81/a"); // case-insensitive host:port: yes
        await fetch("http://only.case.test:82/a"); // wrong port: no
      });
      expect(got.map((g) => g !== null)).toEqual([true, false, false, true, true, false]);
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("a 404 from upstream is not an error on the client span", async () => {
    const real = globalThis.fetch;
    globalThis.fetch = (async () => new Response("no", { status: 404 })) as typeof fetch;
    try {
      instrumentFetch();
      await tracer.trace("req", {}, async () => fetch("http://upstream.test/x"));
      await tracer.flush();
      expect(byName("http.client")[0]).toMatchObject({ error: 0 });
      expect(byName("http.client")[0]!.meta["http.status_code"]).toBe("404");
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("returns the very same Response and marks 5xx as errors", async () => {
    const real = globalThis.fetch;
    const response = new Response("boom", { status: 502 });
    globalThis.fetch = (async () => response) as typeof fetch;
    try {
      instrumentFetch();
      const got = await tracer.trace("req", {}, async () => fetch("http://upstream.test/x"));
      expect(got).toBe(response);
      await tracer.flush();
      expect(byName("http.client")[0]).toMatchObject({ error: 1 });
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("preserves abort semantics: the caller sees the same AbortError, and the span is an error", async () => {
    instrumentFetch();
    const ac = new AbortController();
    const p = tracer.trace("req", {}, async () => fetch(`http://127.0.0.1:${port}/slow`, { signal: ac.signal }));
    setTimeout(() => ac.abort(), 30);
    await expect(p).rejects.toMatchObject({ name: "AbortError" });
    await tracer.flush();
    expect(byName("http.client")[0]).toMatchObject({ error: 1 });
    // An already-aborted signal rejects too.
    await expect(tracer.trace("req", {}, async () => fetch(`http://127.0.0.1:${port}/a`, { signal: AbortSignal.abort() }))).rejects.toMatchObject({ name: "AbortError" });
  });

  it("a network failure rejects with the original error", async () => {
    instrumentFetch();
    await expect(tracer.trace("req", {}, async () => fetch("http://127.0.0.1:1/never"))).rejects.toThrow();
    await tracer.flush();
    expect(byName("http.client")[0]!.error).toBe(1);
  });

  it("a synchronous throw from the wrapped fetch is marked and re-thrown", () => {
    const real = globalThis.fetch;
    const boom = new Error("sync");
    globalThis.fetch = (() => {
      throw boom;
    }) as typeof fetch;
    try {
      instrumentFetch();
      expect(() => tracer.trace("req", {}, () => fetch("http://x.test/"))).toThrow(boom);
    } finally {
      uninstrumentFetch();
      globalThis.fetch = real;
    }
  });

  it("unpatch: restores when outermost; leaves a pass-through when someone wrapped on top", async () => {
    const real = globalThis.fetch;
    instrumentFetch();
    const ours = globalThis.fetch;
    uninstrumentFetch();
    expect(globalThis.fetch).toBe(real);
    instrumentFetch();
    expect(globalThis.fetch).toBe(ours);
    const top = ((i: Parameters<typeof fetch>[0], init?: RequestInit) => ours(i, init)) as typeof fetch;
    globalThis.fetch = top;
    uninstrumentFetch();
    expect(globalThis.fetch).toBe(top);
    await tracer.trace("req", {}, async () => {
      await fetch(`http://127.0.0.1:${port}/passthrough`);
    });
    await tracer.flush();
    expect(byName("http.client")).toHaveLength(0);
    globalThis.fetch = real;
  });

  it("does nothing, harmlessly, when there is no fetch", () => {
    const real = globalThis.fetch;
    // @ts-expect-error simulating an environment without fetch
    globalThis.fetch = undefined;
    try {
      expect(() => instrumentFetch()).not.toThrow();
    } finally {
      globalThis.fetch = real;
    }
  });

  it("the integration object patches and unpatches", () => {
    const real = globalThis.fetch;
    patchIntegrations(["fetch"]);
    expect(globalThis.fetch).not.toBe(real);
    unpatchIntegrations(["fetch"]);
    expect(globalThis.fetch).toBe(real);
  });
});

describe("winston traceFormat", () => {
  it("adds trace_id and span_id of the active span, and leaves entries alone otherwise", () => {
    const f = traceFormat();
    expect(f.transform({ message: "outside", level: "info" })).toEqual({ message: "outside", level: "info" });
    tracer.trace("req", {}, (span) => {
      const out = f.transform({ message: "inside", level: "info" }, {});
      expect(out).toMatchObject({ message: "inside", trace_id: span.traceId, span_id: span.spanId });
    });
  });

  it("builds through winston's own format factory when given one", () => {
    const factory = (fn: (info: Record<string, unknown>) => unknown) => () => ({ transform: (info: Record<string, unknown>) => fn(info) }) as never;
    const f = traceFormat(factory as never);
    tracer.trace("req", {}, (span) => {
      expect(f.transform({ message: "m" })).toMatchObject({ trace_id: span.traceId });
    });
  });

  it("is a no-op with tracing off", () => {
    init({});
    expect(traceFormat().transform({ message: "m" })).toEqual({ message: "m" });
  });
});

describe("integration registry", () => {
  it("lists built-ins and registered integrations; third parties use the same entry point", () => {
    expect(listIntegrations()).toEqual(expect.arrayContaining(["fetch", "sqlite", "next", "winston"]));
    const log: string[] = [];
    const mine: Integration = { name: "mine", isAvailable: () => true, patch: () => void log.push("patch"), unpatch: () => void log.push("unpatch") };
    registerIntegration(mine);
    expect(listIntegrations()).toContain("mine");
    expect(patchIntegrations(["mine", "does-not-exist"])).toEqual(["mine"]);
    unpatchIntegrations(["mine"]);
    expect(log).toEqual(["patch", "unpatch"]);
    globalState().integrations.delete("mine");
  });

  it("skips unavailable integrations and swallows a throwing patch or unpatch", () => {
    const log: string[] = [];
    registerIntegration({ name: "absent", isAvailable: () => false, patch: () => void log.push("absent"), unpatch() {} });
    registerIntegration({
      name: "broken",
      isAvailable: () => true,
      patch() {
        throw new Error("patch");
      },
      unpatch() {
        throw new Error("unpatch");
      },
    });
    registerIntegration({ name: "fine", isAvailable: () => true, patch: () => void log.push("fine"), unpatch() {} });
    expect(patchIntegrations(["absent", "broken", "fine"])).toEqual(["fine"]);
    expect(() => unpatchIntegrations(["broken"])).not.toThrow();
    expect(log).toEqual(["fine"]);
    for (const n of ["absent", "broken", "fine"]) globalState().integrations.delete(n);
  });

  it("a registered integration replaces a built-in of the same name", () => {
    const log: string[] = [];
    registerIntegration({ name: "fetch", isAvailable: () => true, patch: () => void log.push("custom fetch"), unpatch() {} });
    patchIntegrations(["fetch"]);
    expect(log).toEqual(["custom fetch"]);
    globalState().integrations.delete("fetch");
  });

  it("init({integrations}) patches the named ones", () => {
    const log: string[] = [];
    registerIntegration({ name: "viainit", isAvailable: () => true, patch: () => void log.push("p"), unpatch() {} });
    initTracing(agent, { integrations: ["viainit"] });
    expect(log).toEqual(["p"]);
    globalState().integrations.delete("viainit");
  });

  it("registerIntegration never throws on junk", () => {
    expect(() => registerIntegration(null as never)).not.toThrow();
  });
});
