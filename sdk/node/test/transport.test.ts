// UdpTransport with a scripted fake socket, resolver and clock: the failure
// paths a loopback socket cannot produce on demand.
import { describe, expect, it, vi } from "vitest";
import {
  CLOSE_TIMEOUT_MS,
  type Counters,
  DNS_TTL_MS,
  FAILURE_BACKOFF_MS,
  type Lookup,
  MAX_PENDING_PAYLOADS,
  type Payload,
  type SocketLike,
  type TimerFns,
  UdpTransport,
  defaultLookup,
  defaultTimers,
} from "../src/transport.js";

class FakeSocket implements SocketLike {
  connected: string | null = null;
  closed = false;
  sent: string[] = [];
  errorListener: ((err: Error) => void) | null = null;
  connectCallback: (() => void) | null = null;
  sendCallbacks: Array<(err: Error | null) => void> = [];
  autoConnect = true;
  autoAck = true;
  sendThrows = false;
  unrefCalled = false;

  constructor(readonly type: "udp4" | "udp6") {}

  connect(port: number, address: string, callback: () => void): void {
    this.connected = `${address}:${port}`;
    if (this.autoConnect) queueMicrotask(callback);
    else this.connectCallback = callback;
  }
  send(msg: Uint8Array, callback: (err: Error | null) => void): void {
    if (this.sendThrows) throw new Error("boom");
    this.sent.push(Buffer.from(msg).toString());
    if (this.autoAck) queueMicrotask(() => callback(null));
    else this.sendCallbacks.push(callback);
  }
  close(): void {
    if (this.closed) throw new Error("already closed");
    this.closed = true;
  }
  on(_event: "error", listener: (err: Error) => void): this {
    this.errorListener = listener;
    return this;
  }
  unref(): this {
    this.unrefCalled = true;
    return this;
  }
}

const payload = (text: string, messages = 1): Payload => ({ bytes: Buffer.from(text), messages });
const tick = () => new Promise((r) => setTimeout(r, 0));

function setup(opts: { host?: string; lookup?: Lookup; timers?: TimerFns; socket?: (s: FakeSocket) => void } = {}) {
  let now = 1_000_000;
  const counters: Counters = { sent: 0, packets: 0, dropped: 0, errors: 0 };
  const sockets: FakeSocket[] = [];
  const lookups: string[] = [];
  const addresses = ["10.0.0.1"];
  const lookup: Lookup =
    opts.lookup ??
    ((host, cb) => {
      lookups.push(host);
      queueMicrotask(() => cb(null, addresses[0]!, 4));
    });
  const log = vi.fn();
  const transport = new UdpTransport({
    host: opts.host ?? "agent.local",
    port: 8125,
    counters,
    now: () => now,
    createSocket: (type) => {
      const s = new FakeSocket(type);
      opts.socket?.(s);
      sockets.push(s);
      return s;
    },
    lookup,
    timers: opts.timers ?? defaultTimers,
    log,
  });
  return {
    transport,
    counters,
    sockets,
    lookups,
    addresses,
    log,
    advance: (ms: number) => {
      now += ms;
    },
  };
}

describe("UdpTransport", () => {
  it("opens nothing until the first send, then resolves, connects and sends", async () => {
    const t = setup();
    expect(t.transport.hasSocket).toBe(false);
    t.transport.send(payload("a:1|c"));
    await tick();
    expect(t.sockets).toHaveLength(1);
    expect(t.sockets[0]!.type).toBe("udp4");
    expect(t.sockets[0]!.connected).toBe("10.0.0.1:8125");
    expect(t.sockets[0]!.unrefCalled).toBe(true);
    expect(t.sockets[0]!.sent).toEqual(["a:1|c"]);
    expect(t.counters).toEqual({ sent: 1, packets: 1, dropped: 0, errors: 0 });
  });

  it("skips DNS for IP literals and picks the socket family from them", async () => {
    const t = setup({ host: "::1" });
    t.transport.send(payload("a"));
    await tick();
    expect(t.lookups).toEqual([]);
    expect(t.sockets[0]!.type).toBe("udp6");
    expect(t.sockets[0]!.connected).toBe("::1:8125");
  });

  it("resolves at most once per DNS TTL, then reconnects when the address changed", async () => {
    const t = setup();
    t.transport.send(payload("1"));
    await tick();
    t.advance(DNS_TTL_MS - 1);
    t.transport.send(payload("2"));
    await tick();
    expect(t.lookups).toHaveLength(1);

    // TTL expired, same address: refresh in the background, keep the socket.
    t.advance(1);
    t.transport.send(payload("3"));
    await tick();
    expect(t.lookups).toHaveLength(2);
    expect(t.sockets).toHaveLength(1);

    // TTL expired again, address moved: new socket to the new address.
    t.advance(DNS_TTL_MS);
    t.addresses[0] = "10.0.0.2";
    t.transport.send(payload("4"));
    await tick();
    t.transport.send(payload("5"));
    await tick();
    expect(t.lookups).toHaveLength(3);
    expect(t.sockets).toHaveLength(2);
    expect(t.sockets[0]!.closed).toBe(true);
    expect(t.sockets[1]!.connected).toBe("10.0.0.2:8125");
    expect(t.sockets[0]!.sent).toEqual(["1", "2", "3", "4"]);
    expect(t.sockets[1]!.sent).toEqual(["5"]);
    expect(t.counters.sent).toBe(5);
  });

  it("keeps the old address when a refresh lookup fails", async () => {
    let fail = false;
    const t = setup({
      lookup: (_h, cb) => queueMicrotask(() => (fail ? cb(new Error("SERVFAIL"), "", 4) : cb(null, "10.0.0.1", 4))),
    });
    t.transport.send(payload("1"));
    await tick();
    fail = true;
    t.advance(DNS_TTL_MS);
    t.transport.send(payload("2"));
    await tick();
    t.transport.send(payload("3"));
    await tick();
    expect(t.sockets).toHaveLength(1);
    expect(t.sockets[0]!.sent).toEqual(["1", "2", "3"]);
    expect(t.counters.errors).toBe(1);
  });

  it("drops and counts on DNS failure, then backs off before retrying", async () => {
    let calls = 0;
    const t = setup({
      lookup: (_h, cb) => {
        calls++;
        queueMicrotask(() => cb(new Error("ENOTFOUND"), "", 4));
      },
    });
    t.transport.send(payload("a", 2));
    t.transport.send(payload("b", 3));
    await tick();
    expect(t.counters).toEqual({ sent: 0, packets: 0, dropped: 5, errors: 1 });
    expect(t.transport.hasSocket).toBe(false);

    t.transport.send(payload("c"));
    expect(calls).toBe(1); // within the back-off: dropped without a lookup
    expect(t.counters.dropped).toBe(6);

    t.advance(FAILURE_BACKOFF_MS);
    t.transport.send(payload("d"));
    await tick();
    expect(calls).toBe(2);
  });

  it("treats a throwing resolver like a failed lookup", async () => {
    const t = setup({
      lookup: () => {
        throw new Error("resolver exploded");
      },
    });
    t.transport.send(payload("a"));
    await tick();
    expect(t.counters).toMatchObject({ dropped: 1, errors: 1 });
  });

  it("bounds the queue while connecting, dropping the oldest", async () => {
    const t = setup({ socket: (s) => (s.autoConnect = false) });
    for (let i = 0; i < MAX_PENDING_PAYLOADS + 3; i++) t.transport.send(payload(String(i)));
    await tick();
    expect(t.counters.dropped).toBe(3);
    t.sockets[0]!.connectCallback!();
    expect(t.sockets[0]!.sent[0]).toBe("3");
    expect(t.sockets[0]!.sent).toHaveLength(MAX_PENDING_PAYLOADS);
  });

  it("fails over to back-off when the socket errors while connecting", async () => {
    const t = setup({ socket: (s) => (s.autoConnect = false) });
    t.transport.send(payload("a"));
    await tick();
    const s = t.sockets[0]!;
    s.errorListener!(new Error("EACCES"));
    expect(s.closed).toBe(true);
    expect(t.counters).toMatchObject({ dropped: 1, errors: 1 });
    // A late connect callback from the abandoned socket is ignored.
    s.connectCallback!();
    expect(s.sent).toEqual([]);
  });

  it("counts socket errors and send failures without throwing", async () => {
    const t = setup({ socket: (s) => (s.autoAck = false) });
    t.transport.send(payload("a", 2));
    await tick();
    const s = t.sockets[0]!;
    s.sendCallbacks[0]!(new Error("ECONNREFUSED"));
    expect(t.counters).toMatchObject({ sent: 0, dropped: 2, errors: 1 });
    s.errorListener!(new Error("async ICMP error"));
    expect(t.counters.errors).toBe(2);
    s.sendThrows = true;
    expect(() => t.transport.send(payload("b"))).not.toThrow();
    expect(t.counters).toMatchObject({ dropped: 3, errors: 3 });
  });

  it("treats a throwing socket factory as a failed setup", async () => {
    const counters: Counters = { sent: 0, packets: 0, dropped: 0, errors: 0 };
    const transport = new UdpTransport({
      host: "127.0.0.1",
      port: 1,
      counters,
      now: () => 0,
      createSocket: () => {
        throw new Error("EMFILE");
      },
      lookup: defaultLookup,
      timers: defaultTimers,
      log: () => {},
    });
    expect(() => transport.send(payload("a"))).not.toThrow();
    expect(counters).toMatchObject({ dropped: 1, errors: 1 });
  });

  it("close() waits for queued and in-flight sends, then closes the socket", async () => {
    const t = setup({ socket: (s) => (s.autoAck = false) });
    t.transport.send(payload("a"));
    let closed = false;
    const p = t.transport.close().then(() => (closed = true));
    await tick(); // connected, "a" written, ack outstanding
    expect(closed).toBe(false);
    expect(t.sockets[0]!.sent).toEqual(["a"]);
    t.sockets[0]!.sendCallbacks[0]!(null);
    await p;
    expect(t.sockets[0]!.closed).toBe(true);
    expect(t.counters.sent).toBe(1);
    // Closed: further sends are dropped; close() stays resolved.
    t.transport.send(payload("b"));
    expect(t.counters.dropped).toBe(1);
    await t.transport.close();
  });

  it("close() gives up after its timeout and drops what is still queued", async () => {
    let fire: (() => void) | null = null;
    const timers: TimerFns = {
      set: (fn) => {
        fire = fn;
        return {};
      },
      clear: vi.fn(),
    };
    const t = setup({ timers, lookup: () => {} /* never answers */ });
    t.transport.send(payload("a", 4));
    const p = t.transport.close();
    expect(t.transport.close()).toBe(p); // concurrent close() shares the promise
    expect(fire).not.toBeNull();
    fire!();
    await p;
    expect(t.counters.dropped).toBe(4);
    expect(CLOSE_TIMEOUT_MS).toBe(2000);
  });

  it("close() on an idle transport resolves at once", async () => {
    const t = setup();
    await t.transport.close();
    expect(t.sockets).toHaveLength(0);
  });

  it("ignores a lookup answer that arrives after a timed-out close", async () => {
    let answer: (() => void) | null = null;
    let fire: (() => void) | null = null;
    const t = setup({
      lookup: (_h, cb) => (answer = () => cb(null, "10.0.0.1", 4)),
      timers: { set: (fn) => ((fire = fn), {}), clear: () => {} },
    });
    t.transport.send(payload("a"));
    const p = t.transport.close();
    fire!();
    await p;
    answer!();
    await tick();
    expect(t.sockets).toHaveLength(0);
    expect(t.counters.dropped).toBe(1);
  });
});

describe("defaultLookup", () => {
  it("resolves localhost, preferring IPv4", async () => {
    const [err, address, family] = await new Promise<[Error | null, string, number]>((resolve) =>
      defaultLookup("localhost", (e, a, f) => resolve([e, a, f])),
    );
    expect(err).toBeNull();
    expect(family).toBe(4);
    expect(address).toBe("127.0.0.1");
  });

  it("reports resolution failures through the callback", async () => {
    const err = await new Promise<Error | null>((resolve) => defaultLookup("nonexistent.invalid", (e) => resolve(e)));
    expect(err).toBeInstanceOf(Error);
  });
});
