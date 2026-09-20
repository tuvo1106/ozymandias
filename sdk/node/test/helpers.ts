// Shared test fixtures: a real UDP "fake agent" and SDK/env reset helpers.
import * as dgram from "node:dgram";
import { statsd } from "../src/index.js";
import { globalState } from "../src/state.js";

/**
 * A real UDP listener on 127.0.0.1 with an ephemeral port, standing in for the
 * ozymandias agent. It records every datagram exactly as received.
 */
export class FakeAgent {
  readonly datagrams: string[] = [];
  private waiters: Array<() => void> = [];

  private constructor(
    private readonly socket: dgram.Socket,
    readonly port: number,
  ) {
    socket.on("message", (msg) => {
      this.datagrams.push(msg.toString("utf8"));
      const waiters = this.waiters;
      this.waiters = [];
      for (const w of waiters) w();
    });
  }

  static start(): Promise<FakeAgent> {
    return new Promise((resolve, reject) => {
      const socket = dgram.createSocket("udp4");
      socket.once("error", reject);
      socket.bind(0, "127.0.0.1", () => resolve(new FakeAgent(socket, socket.address().port)));
    });
  }

  /** Resolves once `predicate(datagrams)` holds, or rejects after `timeoutMs`. */
  async waitFor(predicate: (datagrams: string[]) => boolean, timeoutMs = 2000): Promise<string[]> {
    const deadline = Date.now() + timeoutMs;
    while (!predicate(this.datagrams)) {
      const left = deadline - Date.now();
      if (left <= 0) throw new Error(`timed out; received ${JSON.stringify(this.datagrams)}`);
      await new Promise<void>((resolve) => {
        const t = setTimeout(resolve, left);
        this.waiters.push(() => {
          clearTimeout(t);
          resolve();
        });
      });
    }
    return this.datagrams;
  }

  /** Waits for `n` datagrams in total and returns them. */
  count(n: number, timeoutMs?: number): Promise<string[]> {
    return this.waitFor((d) => d.length >= n, timeoutMs);
  }

  clear(): void {
    this.datagrams.length = 0;
  }

  close(): Promise<void> {
    return new Promise((resolve) => this.socket.close(() => resolve()));
  }
}

/** A port on 127.0.0.1 with nothing listening on it (bound, then released). */
export async function closedPort(): Promise<number> {
  const agent = await FakeAgent.start();
  const port = agent.port;
  await agent.close();
  return port;
}

const ENV_KEYS = [
  "OZY_AGENT_HOST",
  "OZY_STATSD_PORT",
  "OZY_SERVICE",
  "OZY_ENV",
  "OZY_VERSION",
  "OZY_TAGS",
  "OZY_DEBUG",
];

/** Removes every OZY_* variable and returns a function restoring them. */
export function clearEnv(): () => void {
  const saved = new Map(ENV_KEYS.map((k) => [k, process.env[k]]));
  for (const k of ENV_KEYS) delete process.env[k];
  return () => {
    for (const [k, v] of saved) {
      if (v === undefined) delete process.env[k];
      else process.env[k] = v;
    }
  };
}

/**
 * Closes the current client and leaves the SDK uninitialized. The short pause
 * lets loopback datagrams from the closing flush arrive now, so they cannot
 * leak into the next test's fake agent.
 */
export async function resetSdk(): Promise<void> {
  await statsd.close();
  globalState().statsd = null;
  await new Promise((r) => setTimeout(r, 15));
}

/** Polls `predicate` until true or the timeout elapses. */
export async function eventually(predicate: () => boolean, timeoutMs = 2000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (!predicate()) {
    if (Date.now() > deadline) throw new Error("condition not met in time");
    await new Promise((r) => setTimeout(r, 5));
  }
}
