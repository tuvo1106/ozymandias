/**
 * The UDP transport: turns finished payloads into datagrams to the agent.
 *
 * Why UDP (and why that is fine): a metric call must never slow down or break
 * the app that makes it. A UDP send is fire-and-forget — no connection to
 * establish, no acknowledgement to wait for, no back-pressure when the agent
 * is down. The price is at-most-once delivery: a datagram can be lost. For
 * pre-aggregated metrics that is acceptable; a lost increment moves a rate by
 * a hair, whereas a blocked request handler is an outage.
 *
 * Lifecycle (one transport per client):
 *
 * ```
 *   idle ──send──► resolving ──lookup ok──► connecting ──connect cb──► connected
 *    ▲                 │ lookup failed           │ socket error            │
 *    └─────────────────┴── (drop pending, back off 5 s) ◄──────────────────┘
 *                                                   (IP changed after 60 s: reconnect)
 * ```
 *
 * Design choices, each with the alternative it rejects:
 *
 * - **Lazy socket.** Nothing is opened until the first payload, so an
 *   initialized-but-idle process holds no handle.
 * - **Connected socket** (`socket.connect` then `send` without an address)
 *   rather than addressing every datagram. A connected send skips Node's
 *   per-send DNS lookup and goes to the kernel synchronously, which is what
 *   makes a best-effort flush from a `process.on("exit")` handler possible at
 *   all (after `exit` no further callbacks or ticks run). It also makes the
 *   kernel report ICMP "port unreachable" as a send error we can count.
 * - **Our own DNS cache** (at most one lookup per 60 s, 5 s after a failure).
 *   Re-resolving on every send would put a resolver round trip behind every
 *   datagram; never re-resolving would pin a container IP that changed when
 *   the agent restarted.
 * - **IPv4 preferred.** `localhost` often resolves to `::1` first, while an
 *   agent listening on a v4 socket never sees v6 datagrams — and UDP would
 *   lose them silently. Taking the first v4 address avoids that trap.
 * - **Unref'd socket.** An open UDP socket normally keeps Node's event loop
 *   alive; a metrics client must not be the reason a script never exits.
 * - **Bounded pending queue** while resolving/connecting (drop oldest), and
 *   immediate drops during the post-failure back-off: memory stays bounded
 *   however long the agent is unreachable.
 *
 * Every error — DNS, socket, send — is swallowed and counted. Nothing here
 * throws to the caller.
 *
 * @module
 */
import * as dgram from "node:dgram";
import * as dns from "node:dns";
import { isIP } from "node:net";

/** How long a successful DNS answer is reused, in ms. */
export const DNS_TTL_MS = 60_000;
/** How long after a failed lookup/connect sends are dropped, in ms. */
export const FAILURE_BACKOFF_MS = 5_000;
/** How many payloads may wait while the socket is being set up. */
export const MAX_PENDING_PAYLOADS = 64;
/** Upper bound on how long `close()` waits for in-flight sends, in ms. */
export const CLOSE_TIMEOUT_MS = 2_000;

/** One datagram's bytes plus how many statsd messages it carries. */
export interface Payload {
  bytes: Uint8Array;
  messages: number;
}

/**
 * The subset of `dgram.Socket` the transport uses — narrowed so tests can
 * substitute a fake to simulate failures a real loopback socket can't produce.
 */
export interface SocketLike {
  connect(port: number, address: string, callback: () => void): void;
  send(msg: Uint8Array, callback: (err: Error | null) => void): void;
  close(): void;
  on(event: "error", listener: (err: Error) => void): unknown;
  unref(): unknown;
}

/** Creates a UDP socket of the given family. Default: `dgram.createSocket`. */
export type SocketFactory = (type: "udp4" | "udp6") => SocketLike;

/**
 * Resolves a hostname to one address. Default: `dns.lookup` with all
 * addresses, preferring the first IPv4 one.
 */
export type Lookup = (host: string, callback: (err: Error | null, address: string, family: 4 | 6) => void) => void;

/** A cancellable one-shot timer, as returned by {@link TimerFns.set}. */
export interface TimerHandle {
  unref?(): unknown;
}

/** Timer functions, injectable so tests can observe or control them. */
export interface TimerFns {
  set(fn: () => void, ms: number): TimerHandle;
  clear(handle: TimerHandle): void;
}

/** Counters shared by the transport and the client; see `statsd.stats()`. */
export interface Counters {
  /** Messages handed to the kernel successfully. */
  sent: number;
  /** Datagrams handed to the kernel successfully. */
  packets: number;
  /** Messages discarded: unreachable agent, full queue, send failure, bad value. */
  dropped: number;
  /** Internal errors swallowed: DNS, socket and send failures, caught exceptions. */
  errors: number;
}

/** Everything a transport needs from outside; see {@link UdpTransport}. */
export interface TransportDeps {
  host: string;
  port: number;
  counters: Counters;
  now: () => number;
  createSocket: SocketFactory;
  lookup: Lookup;
  timers: TimerFns;
  log: (msg: string) => void;
}

/** The production socket factory: a real `node:dgram` socket. */
export const defaultCreateSocket: SocketFactory = (type) => dgram.createSocket(type);

/** The production resolver: `dns.lookup`, IPv4 preferred (see module doc). */
export const defaultLookup: Lookup = (host, callback) => {
  dns.lookup(host, { all: true }, (err, addresses) => {
    if (err) return callback(err, "", 4);
    const chosen = addresses.find((a) => a.family === 4) ?? addresses[0];
    if (!chosen) return callback(new Error(`no addresses for ${host}`), "", 4);
    callback(null, chosen.address, chosen.family === 6 ? 6 : 4);
  });
};

/** The production timers: global `setTimeout`, unref'd by the caller. */
export const defaultTimers: TimerFns = {
  set: (fn, ms) => setTimeout(fn, ms),
  clear: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>),
};

type State = "idle" | "resolving" | "connecting" | "connected" | "closed";

/**
 * Sends payloads to one agent address over UDP. Not safe to share between
 * clients; each `StatsdClient` owns one. All methods are synchronous and
 * non-throwing; completion is observable only through the counters.
 */
export class UdpTransport {
  private readonly deps: TransportDeps;
  private state: State = "idle";
  private socket: SocketLike | null = null;
  private family: 4 | 6 = 4;
  private address = "";
  private resolvedAt = Number.NEGATIVE_INFINITY;
  private failedAt = Number.NEGATIVE_INFINITY;
  private refreshing = false;
  private pending: Payload[] = [];
  private inFlight = 0;
  private closing: { resolve: () => void; timer: TimerHandle } | null = null;
  private closePromise: Promise<void> | null = null;

  /** @param deps - host/port, shared counters and injectable effects. */
  constructor(deps: TransportDeps) {
    this.deps = deps;
  }

  /** True once a socket has been created (used by tests and debug output). */
  get hasSocket(): boolean {
    return this.socket !== null;
  }

  /**
   * Queues or sends one payload. Never throws; failures are counted.
   *
   * @param payload - the datagram to send.
   */
  send(payload: Payload): void {
    const c = this.deps.counters;
    if (this.state === "closed") {
      c.dropped += payload.messages;
      return;
    }
    if (this.state === "connected") {
      if (!this.refreshing && this.deps.now() - this.resolvedAt >= DNS_TTL_MS) this.refresh();
      // refresh() may have switched us back to connecting for a new address.
      if (this.state === "connected") {
        this.write(payload);
        return;
      }
    }
    if (this.state === "idle" && this.deps.now() - this.failedAt < FAILURE_BACKOFF_MS) {
      c.dropped += payload.messages;
      return;
    }
    this.pending.push(payload);
    if (this.pending.length > MAX_PENDING_PAYLOADS) {
      c.dropped += this.pending.shift()!.messages; // non-empty: we just pushed
    }
    if (this.state === "idle") this.start();
  }

  /**
   * Waits (at most {@link CLOSE_TIMEOUT_MS}) for queued and in-flight sends,
   * then closes the socket. Payloads still queued at the deadline are dropped
   * and counted. Idempotent.
   *
   * @returns a promise that resolves once the socket is closed; never rejects.
   */
  close(): Promise<void> {
    if (this.closePromise) return this.closePromise;
    if (this.state === "closed") return Promise.resolve();
    this.closePromise = new Promise((resolve) => {
      const timer = this.deps.timers.set(() => this.finishClose(), CLOSE_TIMEOUT_MS);
      timer.unref?.();
      this.closing = { resolve, timer };
      this.maybeFinishClose();
    });
    return this.closePromise;
  }

  private maybeFinishClose(): void {
    if (!this.closing) return;
    if (this.state === "resolving" || this.state === "connecting") return;
    if (this.inFlight > 0) return;
    this.finishClose();
  }

  private finishClose(): void {
    const closing = this.closing;
    // The timeout and a normal finish can race; whichever runs second is a no-op.
    if (!closing) return;
    this.closing = null;
    this.deps.timers.clear(closing.timer);
    this.dropPending();
    this.closeSocket();
    this.state = "closed";
    closing.resolve();
  }

  private start(): void {
    this.state = "resolving";
    this.resolve((err, address, family) => {
      if (this.state !== "resolving") return; // closed meanwhile
      if (err) {
        this.fail(`dns lookup for ${this.deps.host} failed: ${err.message}`);
        return;
      }
      this.connect(address, family);
    });
  }

  private resolve(callback: (err: Error | null, address: string, family: 4 | 6) => void): void {
    const host = this.deps.host;
    const literal = isIP(host);
    if (literal !== 0) {
      callback(null, host, literal === 6 ? 6 : 4);
      return;
    }
    try {
      this.deps.lookup(host, callback);
    } catch (err) {
      callback(toError(err), "", 4);
    }
  }

  private connect(address: string, family: 4 | 6): void {
    this.resolvedAt = this.deps.now();
    this.closeSocket(); // a reconnect to a new address starts from a fresh socket
    this.address = address;
    this.family = family;
    this.state = "connecting";
    try {
      const socket = this.deps.createSocket(family === 6 ? "udp6" : "udp4");
      this.socket = socket;
      // A dgram socket without an 'error' listener turns errors into
      // uncaught exceptions — i.e. it would crash the host app.
      socket.on("error", (err) => this.onSocketError(socket, err));
      socket.unref();
      socket.connect(this.deps.port, address, () => {
        if (this.socket !== socket || this.state !== "connecting") return;
        this.state = "connected";
        this.deps.log(`connected to ${address}:${this.deps.port}`);
        const queued = this.pending;
        this.pending = [];
        for (const p of queued) this.write(p);
        this.maybeFinishClose();
      });
    } catch (err) {
      this.fail(`socket setup failed: ${toError(err).message}`);
    }
  }

  private refresh(): void {
    this.refreshing = true;
    this.resolve((err, address, family) => {
      this.refreshing = false;
      if (this.state !== "connected") return;
      if (err) {
        // Keep using the address we have; try again after another TTL.
        this.resolvedAt = this.deps.now();
        this.countError(`dns refresh for ${this.deps.host} failed: ${err.message}`);
        return;
      }
      if (address === this.address && family === this.family) {
        this.resolvedAt = this.deps.now();
        return;
      }
      this.deps.log(`agent address changed ${this.address} -> ${address}; reconnecting`);
      this.connect(address, family);
    });
  }

  private write(payload: Payload): void {
    const socket = this.socket;
    if (!socket) {
      this.deps.counters.dropped += payload.messages;
      return;
    }
    this.inFlight++;
    try {
      socket.send(payload.bytes, (err) => {
        this.inFlight--;
        if (err) {
          this.deps.counters.dropped += payload.messages;
          this.countError(`send failed: ${err.message}`);
        } else {
          this.deps.counters.sent += payload.messages;
          this.deps.counters.packets++;
        }
        this.maybeFinishClose();
      });
    } catch (err) {
      this.inFlight--;
      this.deps.counters.dropped += payload.messages;
      this.countError(`send threw: ${toError(err).message}`);
    }
  }

  private onSocketError(socket: SocketLike, err: Error): void {
    if (socket !== this.socket) return;
    if (this.state === "connecting") {
      this.fail(`connect failed: ${err.message}`);
      return;
    }
    this.countError(`socket error: ${err.message}`);
  }

  /** Setup failed: count it, drop what was waiting, back off, start over later. */
  private fail(msg: string): void {
    this.countError(msg);
    this.failedAt = this.deps.now();
    this.closeSocket();
    this.dropPending();
    this.state = "idle";
    this.maybeFinishClose();
  }

  private dropPending(): void {
    for (const p of this.pending) this.deps.counters.dropped += p.messages;
    this.pending = [];
  }

  private closeSocket(): void {
    const socket = this.socket;
    this.socket = null;
    if (!socket) return;
    try {
      socket.close();
    } catch {
      // Already closed; nothing to release.
    }
  }

  private countError(msg: string): void {
    this.deps.counters.errors++;
    this.deps.log(msg);
  }
}

function toError(err: unknown): Error {
  return err instanceof Error ? err : new Error(String(err));
}
