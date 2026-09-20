// L10 contract tests: every case in the shared hop-A goldens
// (pkg/wire/testdata/statsd/sdk-cases.json, also loaded by the Go parser tests
// and the Python SDK) must produce exactly the expected datagram on a real UDP
// socket — or nothing at all when `expect` is null.
import { readFileSync } from "node:fs";
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it } from "vitest";
import { type MetricOptions, init, statsd } from "../src/index.js";
import { FakeAgent, clearEnv, resetSdk } from "./helpers.js";

interface SdkCase {
  name: string;
  init: { service?: string; env?: string; version?: string; tags?: string[] };
  call: "increment" | "decrement" | "gauge" | "histogram" | "distribution" | "timing" | "set";
  metric: string;
  value?: number | string;
  value_special?: "nan" | "inf" | "-inf";
  tags?: string[];
  sample_rate?: number;
  random?: number;
  expect: string | null;
}

const goldens = new URL("../../../pkg/wire/testdata/statsd/sdk-cases.json", import.meta.url);
const cases = (JSON.parse(readFileSync(goldens, "utf8")) as { cases: SdkCase[] }).cases;

const SPECIAL: Record<string, number> = { nan: NaN, inf: Infinity, "-inf": -Infinity };

function call(c: SdkCase): void {
  const value = c.value_special !== undefined ? SPECIAL[c.value_special] : c.value;
  const opts: MetricOptions = {};
  if (c.tags) opts.tags = c.tags;
  if (c.sample_rate !== undefined) opts.sampleRate = c.sample_rate;
  switch (c.call) {
    case "increment":
    case "decrement":
      statsd[c.call](c.metric, value as number | undefined, opts);
      return;
    case "set":
      statsd.set(c.metric, value as string | number, opts);
      return;
    default:
      statsd[c.call](c.metric, value as number, opts);
  }
}

describe("hop A contract (sdk-cases.json)", () => {
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

  it("loads the goldens", () => {
    expect(cases.length).toBeGreaterThan(20);
  });

  it.each(cases.map((c) => [c.name, c] as const))("%s", async (_name, c) => {
    init({
      ...c.init,
      agentHost: "127.0.0.1",
      statsdPort: agent.port,
      hooks: { random: () => c.random ?? 0 },
    });
    call(c);
    statsd.flush();
    // A sentinel in its own datagram marks "everything from the call has
    // arrived", which is what makes the `expect: null` cases checkable.
    statsd.increment("sentinel.end");
    statsd.flush();
    const got = await agent.waitFor((d) => d.some((x) => x.startsWith("sentinel.end:")));
    const before = got.slice(0, got.findIndex((x) => x.startsWith("sentinel.end:")));
    expect(before).toEqual(c.expect === null ? [] : [c.expect]);
  });
});
