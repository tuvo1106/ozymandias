// An Express app traced by ozymandias: one middleware at the top, one after the routes.
//
// The app owners' Node service is Next.js; this one exists to prove the Node SDK on the most
// common Node server there is, through the `http` integration:
//
// - `app.use(traceRequests())` makes each request an `http.request` span whose resource is
//   Express's route pattern (`GET /items/:id`), plus the `http.request.count` / `.duration`
//   metrics. The span is active while the handlers run, across `await`.
// - `instrumentFetch({ propagateTo })` turns an outgoing `fetch` into an `http.client` span and,
//   for an allow-listed host only, sends the trace headers with it. Here the item route calls
//   the app's own `/internal/price/:id`, so one trace holds a request, the call it made and the
//   request that call became: the shape of a call between two services.
// - `app.use(traceErrors())` after the routes hands the error behind Express's 500 page to the
//   span, which Express would otherwise swallow.
//
// Run it (Node >= 22; `npm install` here first, which also builds the SDK next to it):
//
//   OZY_AGENT_HOST=localhost node server.mjs           # serve on :5060
//   OZY_AGENT_HOST=localhost node server.mjs --drive   # serve on a free port, send a few
//                                                      # requests through it, print the trace id
//
// With OZY_AGENT_HOST unset the SDK is inert and the app works exactly the same.
import express from "express";
import { init, instrumentFetch, statsd, traceErrors, traceRequests, tracer } from "ozy";

init({ service: "express-example", env: "dev" });
instrumentFetch({ propagateTo: ["127.0.0.1"] });

const ITEMS = new Map([
  [1, "anvil"],
  [2, "bellows"],
  [3, "crucible"],
]);

const app = express();
app.use(traceRequests({ excludePaths: ["/healthz"] }));

app.get("/items", (_req, res) => {
  res.json([...ITEMS].map(([id, name]) => ({ id, name })));
});

app.get("/items/:id", async (req, res) => {
  const id = Number(req.params.id);
  const name = ITEMS.get(id);
  if (!name) {
    res.status(404).json({ error: "no such item" });
    return;
  }
  // Calls this same server, standing in for a second service.
  const price = await fetch(`http://127.0.0.1:${req.socket.localPort}/internal/price/${id}`).then((r) => r.json());
  res.json({ id, name, ...price });
});

app.get("/internal/price/:id", async (req, res) => {
  // A unit of work worth its own span: tracer.trace is what an integration would call.
  const cents = await tracer.trace("price.lookup", { resource: "price.lookup", type: "custom" }, async () => {
    await new Promise((resolve) => setTimeout(resolve, 5));
    return 100 * Number(req.params.id) + 99;
  });
  res.json({ cents });
});

app.get("/boom", async () => {
  throw new Error("the example failed on purpose");
});

app.get("/healthz", (_req, res) => {
  res.send("ok");
});

app.use(traceErrors());
// Express's default error handler answers 500 and logs the stack; that is fine for an example.

async function drive() {
  const server = app.listen(0, "127.0.0.1");
  await new Promise((resolve) => server.once("listening", resolve));
  const base = `http://127.0.0.1:${server.address().port}`;
  const statuses = {};
  // One trace for the whole drive: fetch is instrumented, so each request carries the
  // driver's span in its headers, the way one service calls another.
  const traceId = await tracer.trace("example.drive", { resource: "express-example drive" }, async (span) => {
    for (const path of ["/items", "/items/1", "/items/2", "/items/999", "/nope/42", "/boom"]) {
      const res = await fetch(base + path);
      await res.arrayBuffer();
      statuses[path] = res.status;
    }
    return span.context().traceId;
  });
  server.close();
  await tracer.flush();
  statsd.flush();
  console.log(JSON.stringify({ trace_id: traceId, statuses }));
}

if (process.argv.includes("--drive")) {
  await drive();
} else {
  app.listen(5060, "127.0.0.1", () => console.log("listening on http://127.0.0.1:5060"));
}
