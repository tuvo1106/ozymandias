// The http integration (traceRequests / traceErrors) against a real Express 5
// server and a plain node:http one, over real sockets.
import * as http from "node:http";
import type { AddressInfo } from "node:net";
import express from "express";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from "vitest";
import { expressRoute, init, patchIntegrations, statsd, traceErrors, traceRequests, tracer, unpatchIntegrations } from "../src/index.js";
import type { TraceRequestsOptions } from "../src/integrations/http.js";
import { globalState } from "../src/state.js";
import { clearEnv, FakeAgent } from "./helpers.js";
import { FakeTraceAgent, resetTracing } from "./trace-helpers.js";

const SECRET = "sentinel-secret-7731";

let traces: FakeTraceAgent;
let udp: FakeAgent;
let restoreEnv: () => void;
beforeAll(async () => {
  traces = await FakeTraceAgent.start();
  udp = await FakeAgent.start();
});
afterAll(async () => {
  await traces.close();
  await udp.close();
});
beforeEach(() => {
  restoreEnv = clearEnv();
  traces.clear();
  udp.clear();
});
afterEach(async () => {
  patchIntegrations(["http"]); // its default is on; unpatch only switches it off
  await resetTracing();
  restoreEnv();
});

function initBoth(extra: Parameters<typeof init>[0] = {}): void {
  init({ service: "svc", env: "test", agentHost: "127.0.0.1", tracePort: traces.port, statsdPort: udp.port, ...extra });
}

/** Serves `handler` on an ephemeral port for the duration of `fn`. */
async function serve<T>(handler: http.RequestListener, fn: (base: string) => Promise<T>): Promise<T> {
  const server = http.createServer(handler);
  await new Promise<void>((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    return await fn(`http://127.0.0.1:${(server.address() as AddressInfo).port}`);
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
  }
}

function build(opts: TraceRequestsOptions = {}): express.Express {
  const app = express();
  app.use(traceRequests(opts));
  const users = express.Router({ mergeParams: true });
  users.get("/:id", async (req, res) => {
    await tracer.trace("handler.work", async () => {
      await new Promise((r) => setTimeout(r, 2));
    });
    res.json({ org: (req.params as Record<string, string>)["org"], id: req.params.id });
  });
  users.get("/", (_req, res) => {
    res.send("list");
  });
  const plain = express.Router(); // no mergeParams: the org is not in req.params
  plain.get("/:id", (_req, res) => {
    res.send("plain");
  });
  app.use("/orgs/:org/users", users);
  app.use("/teams/:team/members", plain);
  app.use("/api/v1/users", users);
  app.get("/items/:itemId", (_req, res) => {
    res.send("item");
  });
  app.post("/boom", async () => {
    throw new Error("handler exploded");
  });
  app.get("/unavailable", (_req, res) => {
    res.status(503).send("later");
  });
  app.get("/teapot", (_req, res) => {
    res.status(418).send("short and stout");
  });
  app.get("/health", (_req, res) => {
    res.send("ok");
  });
  app.get("/slow", (_req, res) => {
    setTimeout(() => res.send("late"), 200);
  });
  app.get("/stream", (_req, res) => {
    res.writeHead(200, { "content-type": "text/plain" });
    res.write("first chunk\n");
    setTimeout(() => res.end("done"), 200);
  });
  app.use(traceErrors());
  return app;
}

const requestSpans = () => traces.spans().filter((s) => s.name === "http.request");

/** Every metric line received, parsed: never searched as text (the duration holds a measured number). */
function metrics(): Array<{ name: string; type: string; tags: Record<string, string> }> {
  return udp.datagrams
    .flatMap((d) => d.split("\n"))
    .filter(Boolean)
    .map((line) => {
      const [head, tagPart] = line.split("|#");
      const [nameValue, type] = head!.split("|");
      const tags = Object.fromEntries((tagPart ?? "").split(",").filter(Boolean).map((t) => [t.slice(0, t.indexOf(":")), t.slice(t.indexOf(":") + 1)]));
      return { name: nameValue!.split(":")[0]!, type: type!, tags };
    });
}

const counts = () => metrics().filter((m) => m.name === "http.request.count").map((m) => m.tags);

async function settle(nCounts: number): Promise<void> {
  statsd.flush();
  await udp.waitFor(() => counts().length >= nCounts);
  await tracer.flush();
}

describe("traceRequests with Express", () => {
  it("records one web span with the route pattern, and the metrics agree", async () => {
    initBoth();
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/items/42?token=${SECRET}`)).status).toBe(200);
    });
    await settle(1);
    const [span] = requestSpans();
    expect(span).toMatchObject({ type: "web", resource: "GET /items/:itemId", parent_id: null, error: 0 });
    expect(span!.meta).toMatchObject({
      "http.method": "GET",
      "http.route": "/items/:itemId",
      "http.status_code": "200",
      "http.url": "/items/42",
      "span.kind": "server",
    });
    expect(span!.metrics["_top_level"]).toBe(1);
    expect(traces.raw.join("")).not.toContain(SECRET);
    expect(counts()).toEqual([
      expect.objectContaining({ route: "/items/:itemId", method: "GET", status: "200", status_class: "2xx" }),
    ]);
    expect(metrics().map((m) => [m.name, m.type])).toEqual([
      ["http.request.count", "c"],
      ["http.request.duration", "d"],
    ]);
  });

  it("puts a router's base in front of the pattern, with parameter values put back", async () => {
    initBoth();
    await serve(build(), async (base) => {
      await fetch(`${base}/api/v1/users/7`);
      await fetch(`${base}/api/v1/users`);
      await fetch(`${base}/orgs/acme/users/7`); // mergeParams: org is known
      await fetch(`${base}/orgs/123/users/7`);
      await fetch(`${base}/orgs/7/users/7`); // one value, two params: the base's own param wins
      await fetch(`${base}/teams/42/members/7`); // no mergeParams: only an id-like value is caught
    });
    await settle(6);
    expect(requestSpans().map((s) => s.resource)).toEqual([
      "GET /api/v1/users/:id",
      "GET /api/v1/users",
      "GET /orgs/:org/users/:id",
      "GET /orgs/:org/users/:id",
      "GET /orgs/:org/users/:id",
      "GET /teams/:id/members/:id",
    ]);
  });

  it("makes the handler's spans children of the request, across await", async () => {
    initBoth();
    await serve(build(), async (base) => {
      await fetch(`${base}/api/v1/users/1`);
      await fetch(`${base}/api/v1/users/2`);
    });
    await settle(2);
    const chunks = traces.chunks();
    expect(chunks).toHaveLength(2); // one trace per request
    for (const chunk of chunks) {
      const root = chunk.find((s) => s.name === "http.request")!;
      const work = chunk.find((s) => s.name === "handler.work")!;
      expect([work.trace_id, work.parent_id]).toEqual([root.trace_id, root.span_id]);
    }
    expect(chunks[0]![0]!.trace_id).not.toBe(chunks[1]![0]!.trace_id);
    expect(tracer.scope().active()).toBeNull();
  });

  it("falls back to the normalized path when nothing matched, and a 404 is not an error", async () => {
    initBoth();
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/nope/123/abc`)).status).toBe(404);
    });
    await settle(1);
    const [span] = requestSpans();
    expect(span!.resource).toBe("GET /nope/:id/abc");
    expect(span!.meta["http.route"]).toBeUndefined();
    expect(span!.error).toBe(0);
    expect(counts()[0]!.route).toBe("unmatched");
  });

  it("names the error behind a 500 page when traceErrors is added", async () => {
    initBoth();
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/boom`, { method: "POST" })).status).toBe(500);
    });
    await settle(1);
    const [span] = requestSpans();
    expect(span).toMatchObject({ error: 1, resource: "POST /boom" });
    expect(span!.meta).toMatchObject({ "http.status_code": "500", "error.type": "Error", "error.message": "handler exploded" });
  });

  it("marks a 5xx with no exception as an error, and a 4xx as not", async () => {
    initBoth();
    await serve(build(), async (base) => {
      await fetch(`${base}/unavailable`);
      await fetch(`${base}/teapot`);
    });
    await settle(2);
    const [unavailable, teapot] = requestSpans();
    expect(unavailable!.error).toBe(1);
    expect(unavailable!.meta["error.message"]).toBe("HTTP 503");
    expect(teapot!.error).toBe(0);
  });

  it("continues an upstream trace from the propagation headers", async () => {
    initBoth();
    await serve(build(), async (base) => {
      await fetch(`${base}/items/1`, {
        headers: {
          "x-ozy-trace-id": "ABCDEF0123456789ABCDEF0123456789",
          "x-ozy-parent-id": "0123456789abcdef",
          "x-ozy-sampling-priority": "2",
        },
      });
      await fetch(`${base}/items/2`, { headers: { "x-ozy-trace-id": "short", "x-ozy-parent-id": "0123456789abcdef" } });
    });
    await settle(2);
    const [continued, fresh] = requestSpans();
    expect(continued).toMatchObject({ trace_id: "abcdef0123456789abcdef0123456789", parent_id: "0123456789abcdef" });
    expect(continued!.metrics["_sampling_priority"]).toBe(2);
    expect(fresh!.parent_id).toBeNull();
  });

  it("keeps hostile methods, paths and route counts bounded", async () => {
    initBoth();
    const app = build({ maxRoutes: 3 });
    await serve(app, async (base) => {
      // Methods Node's parser knows but no one should get a series for (it rejects unknown tokens itself).
      for (const method of ["PROPFIND", "MKCOL", "PURGE"]) await fetch(`${base}/items/1`, { method });
      for (let i = 0; i < 20; i++) await fetch(`${base}/scan/${i}/${i * 7919}/x`);
      // Four distinct matched routes against a cap of three.
      for (const p of ["/items/1", "/health", "/teapot", "/unavailable", "/items/2"]) await fetch(base + p);
    });
    await settle(28);
    const tags = counts();
    expect(new Set(tags.map((t) => t.method))).toEqual(new Set(["OTHER", "GET"]));
    expect(tags.at(-1)!.route).toBe("/items/:itemId"); // a route seen before the cap keeps its name
    const routes = new Set(tags.map((t) => t.route));
    expect(routes).toEqual(new Set(["/items/:itemId", "unmatched", "/health", "/teapot", "other"]));
    const overflow = requestSpans().find((s) => s.meta["http.url"] === "/unavailable")!;
    expect(overflow.resource).toBe("GET /unavailable"); // the normalized path, as for unmatched
    expect(overflow.meta["http.route"]).toBeUndefined();
    const resources = new Set(requestSpans().map((s) => s.resource));
    expect(resources.has("GET /scan/:id/:id/x")).toBe(true);
  });

  it("records 499 for a client that left before any header, not an error", async () => {
    initBoth();
    await serve(build(), async (base) => {
      const ctl = new AbortController();
      const pending = fetch(`${base}/slow`, { signal: ctl.signal }).catch(() => undefined);
      await new Promise((r) => setTimeout(r, 50));
      ctl.abort();
      await pending;
      await new Promise((r) => setTimeout(r, 250)); // the handler answers into a closed socket
    });
    await settle(1);
    const [span] = requestSpans();
    expect(span!.meta["http.status_code"]).toBe("499");
    expect(span!.error).toBe(0);
    expect(counts()).toEqual([expect.objectContaining({ status: "499", status_class: "4xx" })]);
  });

  it("keeps the status already sent when the client leaves mid-body", async () => {
    initBoth();
    await serve(build(), async (base) => {
      await new Promise<void>((resolve) => {
        const req = http.get(`${base}/stream`, (res) => {
          res.once("data", () => {
            req.destroy();
            resolve();
          });
        });
        req.on("error", () => undefined);
      });
      await new Promise((r) => setTimeout(r, 50));
    });
    await settle(1);
    expect(counts()[0]!.status).toBe("200");
    expect(requestSpans()).toHaveLength(1);
  });

  it("times a streamed response to its end", async () => {
    initBoth();
    await serve(build(), async (base) => {
      expect(await (await fetch(`${base}/stream`)).text()).toBe("first chunk\ndone");
    });
    await settle(1);
    expect(requestSpans()[0]!.duration).toBeGreaterThanOrEqual(150_000); // microseconds
  });

  it("leaves excluded paths untraced and uncounted, for every middleware further in", async () => {
    initBoth();
    const app = express();
    app.use(traceRequests({ excludePaths: "/health" }));
    app.use(traceRequests()); // would trace it if the exclusion did not carry
    app.get("/health", (_req, res) => {
      res.send("ok");
    });
    app.get("/x", (_req, res) => {
      res.send("x");
    });
    await serve(app, async (base) => {
      await fetch(`${base}/health?probe=1`);
      await fetch(`${base}/x`);
    });
    await settle(1);
    expect(requestSpans().map((s) => s.resource)).toEqual(["GET /x"]);
    expect(counts()).toHaveLength(1);
  });

  it("counts a request once when two middlewares see it", async () => {
    initBoth();
    const app = express();
    app.use(traceRequests());
    app.use(traceRequests());
    app.get("/once", (_req, res) => {
      res.send("ok");
    });
    await serve(app, async (base) => {
      await fetch(`${base}/once`);
    });
    await settle(1);
    await new Promise((r) => setTimeout(r, 50));
    expect(counts()).toHaveLength(1);
    expect(requestSpans()).toHaveLength(1);
  });

  it("still counts with tracing off, and still traces with metrics off", async () => {
    initBoth({ traceEnabled: false });
    await serve(build(), async (base) => {
      await fetch(`${base}/items/3`);
    });
    statsd.flush();
    await udp.waitFor(() => counts().length >= 1);
    expect(counts()[0]!.route).toBe("/items/:itemId");
    expect(traces.spans()).toEqual([]);
    await resetTracing();
    udp.clear();

    initBoth();
    globalState().statsd = null; // metrics off, tracing on
    await serve(build(), async (base) => {
      await fetch(`${base}/items/4`);
    });
    await tracer.flush();
    expect(requestSpans()[0]!.resource).toBe("GET /items/:itemId");
  });

  it("only calls next() when the SDK is off or the integration is unpatched", async () => {
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/items/1`)).status).toBe(200);
    });
    initBoth();
    unpatchIntegrations(["http"]);
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/items/1`)).status).toBe(200);
    });
    statsd.flush();
    await tracer.flush();
    expect(traces.spans()).toEqual([]);
    expect(counts()).toEqual([]);
  });

  it("accepts a route function for other frameworks, and survives one that throws", async () => {
    initBoth();
    const named = traceRequests({ route: (req) => (req.url?.startsWith("/w/") ? "/w/{name}" : undefined) });
    const broken = traceRequests({
      route: () => {
        throw new Error("bad route fn");
      },
    });
    const handler = (_req: http.IncomingMessage, res: http.ServerResponse) => {
      res.end("ok");
    };
    await serve(
      (req, res) => {
        const mw = req.url?.startsWith("/b") ? broken : named;
        mw(req, res, () => handler(req, res));
      },
      async (base) => {
        await fetch(`${base}/w/alice`);
        await fetch(`${base}/b/9`);
      },
    );
    await settle(2);
    expect(requestSpans().map((s) => s.resource)).toEqual(["GET /w/{name}", "GET /b/:id"]);
    expect(counts().map((t) => t.route)).toEqual(["/w/{name}", "unmatched"]);
  });

  it("custom metric names", async () => {
    initBoth();
    await serve(build({ countName: "web.n", durationName: "web.ms" }), async (base) => {
      await fetch(`${base}/health`);
    });
    statsd.flush();
    await udp.waitFor(() => metrics().length >= 2);
    expect(metrics().map((m) => [m.name, m.type])).toEqual([
      ["web.n", "c"],
      ["web.ms", "d"],
    ]);
  });
});

describe("traceRequests on a plain node:http server", () => {
  it("records a request, and a synchronous handler error passes through marked", async () => {
    initBoth();
    const mw = traceRequests();
    const thrown: unknown[] = [];
    await serve(
      (req, res) => {
        try {
          mw(req, res, () => {
            if (req.url === "/throw") throw new Error("sync boom");
            res.statusCode = 201;
            res.end("made");
          });
        } catch (err) {
          thrown.push(err);
          res.statusCode = 500;
          res.end();
        }
      },
      async (base) => {
        expect((await fetch(`${base}/made/5`)).status).toBe(201);
        expect((await fetch(`${base}/throw`)).status).toBe(500);
      },
    );
    await settle(2);
    expect(thrown).toEqual([new Error("sync boom")]);
    const [made, threw] = requestSpans();
    expect(made!.resource).toBe("GET /made/:id");
    expect(threw).toMatchObject({ error: 1 });
    expect(threw!.meta["error.message"]).toBe("sync boom");
  });

  it("records an invalid status as 500, never a new series", async () => {
    initBoth();
    const mw = traceRequests();
    await serve(
      (req, res) => {
        mw(req, res, () => {
          res.end("ok");
          // After the response went out a buggy handler scribbles on statusCode.
          (res as { statusCode: number }).statusCode = 999;
        });
      },
      async (base) => {
        await fetch(`${base}/x`);
      },
    );
    await settle(1);
    expect(counts()[0]!.status).toBe("500");
  });

  it("never lets a recording failure reach the request", async () => {
    initBoth();
    const client = globalState().statsd!;
    const spy = vi.spyOn(client, "metric").mockImplementation(() => {
      throw new Error("statsd is broken");
    });
    const startSpan = vi.spyOn(tracer, "startSpan").mockImplementation(() => {
      throw new Error("tracer is broken");
    });
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/health`)).status).toBe(200);
    });
    startSpan.mockRestore();
    await serve(build(), async (base) => {
      expect((await fetch(`${base}/health`)).status).toBe(200);
    });
    expect(spy).toHaveBeenCalled();
    spy.mockRestore();
  });
});

describe("expressRoute", () => {
  const req = (fields: Record<string, unknown>) => fields as unknown as http.IncomingMessage;
  it("reads the path from the original url, with no query, through the middleware", async () => {
    initBoth();
    const mw = traceRequests();
    const run = (url: string | undefined, originalUrl?: string) =>
      new Promise<void>((resolve) => {
        const res = new http.ServerResponse({ method: "GET" } as http.IncomingMessage);
        mw({ url, originalUrl, method: "GET", headers: {} } as unknown as http.IncomingMessage, res, () => {
          res.emit("finish");
          resolve();
        });
      });
    await run(undefined);
    await run("?only=query");
    await run("/ignored", "/real/path#frag");
    await tracer.flush();
    expect(requestSpans().map((s) => s.meta["http.url"])).toEqual(["/", "/", "/real/path"]);
  });
  it("is undefined without a string route pattern", () => {
    expect(expressRoute(req({}))).toBeUndefined();
    expect(expressRoute(req({ route: { path: ["/a", "/b"] } }))).toBeUndefined();
    expect(expressRoute(req({ route: { path: /re/ } }))).toBeUndefined();
  });
  it("joins the base and the pattern, and a root pattern adds nothing", () => {
    expect(expressRoute(req({ baseUrl: "/api", route: { path: "/x/:id" } }))).toBe("/api/x/:id");
    expect(expressRoute(req({ baseUrl: "/api", route: { path: "/" } }))).toBe("/api");
    expect(expressRoute(req({ baseUrl: "", route: { path: "/" } }))).toBe("/");
  });
  it("tolerates odd params", () => {
    expect(expressRoute(req({ baseUrl: "/o/acme", params: null, route: { path: "/" } }))).toBe("/o/acme");
    expect(expressRoute(req({ baseUrl: "/o/acme", params: { org: 5, x: "" }, route: { path: "/" } }))).toBe("/o/acme");
  });
});

describe("traceErrors", () => {
  it("passes the error on unchanged, even on a frozen request", () => {
    const next = vi.fn();
    const err = new Error("e");
    traceErrors()(err, Object.freeze({}) as http.IncomingMessage, {} as http.ServerResponse, next);
    expect(next).toHaveBeenCalledWith(err);
  });
});
