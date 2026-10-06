// Tracer overhead: per-span time and allocation with tracing disabled,
// enabled-unsampled (rate 0) and enabled-sampled (rate 1). M5 spec L12 sets
// no budget; the point is to record the number so a regression is visible.
//
//   npm run build && node --expose-gc --max-semi-space-size=256 scripts/bench-trace.mjs
//
// How it measures, and why:
// - Time: only the synchronous slices around `trace()` are timed; the awaits
//   that let the writer drain to a local sink between slices are not, so the
//   number is the cost the host's request pays, not the cost of HTTP.
// - Allocation: heapUsed growth over a fresh batch with a semi-space large
//   enough that no scavenge runs in between, so the delta is bytes allocated
//   (or retained, for the writer's serialized chunk) per span.
// - "root": every trace is one span (one chunk per span, the worst case for
//   the writer). "child": a root with 9 children, reported per span.
import * as http from "node:http";
import { pathToFileURL } from "node:url";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const dist = join(dirname(fileURLToPath(import.meta.url)), "..", "dist", "esm", "index.js");
const { init, tracer } = await import(pathToFileURL(dist).href);

const sink = http.createServer((req, res) => {
  req.resume();
  req.on("end", () => res.end('{"rate_by_service":{}}'));
});
await new Promise((r) => sink.listen(0, "127.0.0.1", r));
const port = sink.address().port;

const SLICE = 500;
const SLICES = 200;

async function measure(label, shape) {
  const slices = [];
  for (let s = 0; s < SLICES; s++) {
    const t0 = process.hrtime.bigint();
    for (let i = 0; i < SLICE; i++) shape();
    slices.push(Number(process.hrtime.bigint() - t0));
    await tracer.flush();
  }
  slices.sort((a, b) => a - b);
  const spansPerCall = shape.spans;
  const per = (ns) => (ns / (SLICE * spansPerCall) / 1000).toFixed(2);
  // Allocation: one small batch with no GC in between.
  globalThis.gc?.();
  const before = process.memoryUsage().heapUsed;
  for (let i = 0; i < 2000; i++) shape();
  const after = process.memoryUsage().heapUsed;
  await tracer.flush();
  const bytes = Math.round((after - before) / (2000 * spansPerCall));
  console.log(
    `${label.padEnd(34)} median ${per(slices[SLICES >> 1]).padStart(6)} us/span   p90 ${per(slices[Math.floor(SLICES * 0.9)]).padStart(6)} us/span   ~${String(bytes).padStart(5)} B/span`,
  );
}

const root = () => tracer.trace("bench", { type: "custom" }, () => 1);
root.spans = 1;
const tree = () =>
  tracer.trace("bench", {}, () => {
    for (let i = 0; i < 9; i++) tracer.trace("child", {}, () => 1);
  });
tree.spans = 10;
const bare = () => 1;
bare.spans = 1;

console.log(`node ${process.version}, ${SLICES} x ${SLICE} calls per shape\n`);

await measure("baseline: empty function", bare);

init({});
await measure("disabled: root", root);
await measure("disabled: 10-span trace", tree);

for (const [name, rate] of [
  ["enabled, unsampled (rate 0)", 0],
  ["enabled, sampled (rate 1)", 1],
]) {
  init({ service: "bench", env: "dev", agentHost: "127.0.0.1", tracePort: port, traceSampleRate: rate });
  await measure(`${name}: root`, root);
  await measure(`${name}: 10-span trace`, tree);
}

init({});
sink.closeAllConnections();
sink.close();
