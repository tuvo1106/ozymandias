import { fetchHealth, formatUptime } from "./health";

const ok = { status: "ok", component: "ozyd", version: "v0.1.0", uptime_seconds: 42 };

function fakeFetch(respond: () => Promise<Response>): typeof fetch {
  return (() => respond()) as unknown as typeof fetch;
}

describe("fetchHealth", () => {
  it("returns a valid health document", async () => {
    const f = fakeFetch(async () => new Response(JSON.stringify(ok), { status: 200 }));
    await expect(fetchHealth(f)).resolves.toEqual(ok);
  });

  it("explains an unreachable server", async () => {
    const f = fakeFetch(async () => {
      throw new TypeError("Failed to fetch");
    });
    await expect(fetchHealth(f)).rejects.toThrow("ozyd is unreachable: Failed to fetch");
  });

  it("describes non-Error rejections too", async () => {
    const f = fakeFetch(() => Promise.reject("boom"));
    await expect(fetchHealth(f)).rejects.toThrow("unreachable: boom");
  });

  it("passes aborts through untouched so callers can ignore them", async () => {
    const f = fakeFetch(async () => {
      throw new DOMException("aborted", "AbortError");
    });
    await expect(fetchHealth(f)).rejects.toMatchObject({ name: "AbortError" });
  });

  it("reports the HTTP status of a failing server", async () => {
    const f = fakeFetch(async () => new Response("down", { status: 503, statusText: "Service Unavailable" }));
    await expect(fetchHealth(f)).rejects.toThrow("ozyd answered 503 Service Unavailable");
  });

  it.each([
    ["not JSON", "<html>"],
    ["null", "null"],
    ["a wrong shape", JSON.stringify({ ...ok, uptime_seconds: "42" })],
  ])("rejects a body that is %s", async (_, body) => {
    const f = fakeFetch(async () => new Response(body, { status: 200 }));
    await expect(fetchHealth(f)).rejects.toThrow("unexpected /healthz response");
  });
});

describe("formatUptime", () => {
  it.each([
    [0, "0s"],
    [45, "45s"],
    [200, "3m 20s"],
    [3600, "1h"],
    [5 * 3600 + 2 * 60 + 9, "5h 2m"],
    [12 * 86400 + 4 * 3600, "12d 4h"],
    [86400 + 59, "1d"],
    [59.9, "59s"],
  ])("formats %s seconds as %s", (secs, want) => {
    expect(formatUptime(secs)).toBe(want);
  });

  it.each([-1, Number.NaN, Number.POSITIVE_INFINITY])("shows a dash for %s", (secs) => {
    expect(formatUptime(secs)).toBe("—");
  });
});
