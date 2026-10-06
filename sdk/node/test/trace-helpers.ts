// Fixtures for the tracing tests: a real HTTP "fake agent" for POST /v1/traces
// and an init/reset pair that points the tracer at it.
import * as http from "node:http";
import { init } from "../src/index.js";
import { globalState } from "../src/state.js";
import { resetSdk } from "./helpers.js";

/** One span as the agent sees it on the wire. */
export interface SeenSpan {
  trace_id: string;
  span_id: string;
  parent_id: string | null;
  service: string;
  name: string;
  resource: string;
  type: string;
  start: number;
  duration: number;
  error: number;
  meta: Record<string, string>;
  metrics: Record<string, number>;
}

/** A real HTTP listener standing in for the agent's `POST /v1/traces`. */
export class FakeTraceAgent {
  readonly bodies: Array<{ tracer: { lang: string; lang_version: string; version: string }; traces: SeenSpan[][] }> = [];
  readonly raw: string[] = [];
  status = 200;
  /** Sent back as `rate_by_service`. */
  rates: Record<string, number> = {};
  /** When true the server never answers (to exercise the client timeout). */
  hang = false;

  private constructor(
    private readonly server: http.Server,
    readonly port: number,
  ) {}

  static start(): Promise<FakeTraceAgent> {
    return new Promise((resolve, reject) => {
      let agent: FakeTraceAgent;
      const server = http.createServer((req, res) => {
        const parts: Buffer[] = [];
        req.on("data", (d: Buffer) => parts.push(d));
        req.on("end", () => {
          const text = Buffer.concat(parts).toString("utf8");
          agent.raw.push(text);
          if (req.url === "/v1/traces" && req.method === "POST") agent.bodies.push(JSON.parse(text));
          if (agent.hang) return;
          res.statusCode = agent.status;
          res.setHeader("content-type", "application/json");
          res.end(JSON.stringify({ rate_by_service: agent.rates, accepted: 0, rejected: 0 }));
        });
      });
      server.once("error", reject);
      server.listen(0, "127.0.0.1", () => {
        agent = new FakeTraceAgent(server, (server.address() as { port: number }).port);
        resolve(agent);
      });
    });
  }

  /** Every chunk received so far. */
  chunks(): SeenSpan[][] {
    return this.bodies.flatMap((b) => b.traces);
  }

  /** Every span received so far, flattened. */
  spans(): SeenSpan[] {
    return this.chunks().flat();
  }

  clear(): void {
    this.bodies.length = 0;
    this.raw.length = 0;
  }

  close(): Promise<void> {
    this.server.closeAllConnections();
    return new Promise((resolve) => this.server.close(() => resolve()));
  }
}

/** Initializes the SDK with tracing pointed at `agent`. */
export function initTracing(agent: FakeTraceAgent, extra: Parameters<typeof init>[0] = {}): void {
  init({ service: "svc", env: "test", version: "1.2.3", agentHost: "127.0.0.1", tracePort: agent.port, ...extra });
}

/** Tears the SDK (and tracer) down between tests. */
export async function resetTracing(): Promise<void> {
  await resetSdk();
  globalState().fetchAllow = [];
}
