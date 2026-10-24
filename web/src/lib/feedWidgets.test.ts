import { describe, expect, it } from "vitest";
import { DEFAULT_SUMMARY_LIMIT, listParams, monitorByUid, selectMonitors, serviceCards, serviceEnvStatus, taggedMonitors } from "./feedWidgets";
import type { GroupState, Status, StoredMonitor } from "./monitorsApi";

let next = 1;
function mon(name: string, status: Status | null, extra: Partial<StoredMonitor> = {}): StoredMonitor {
  return {
    id: next++,
    name,
    type: "metric",
    query: "avg(last_5m):avg:x{*} > 1",
    options: {},
    message: "",
    tags: [],
    enabled: true,
    provisioned: false,
    created_at: "",
    updated_at: "",
    muted_until: null,
    groups: {},
    status,
    ...extra,
  } as StoredMonitor;
}

describe("selectMonitors", () => {
  const ms = [mon("b ok", "OK"), mon("a alert", "ALERT"), mon("c none", null), mon("d warn", "WARN"), mon("e nodata", "NO_DATA")];

  it("sorts worst first and keeps everything when no status is asked for", () => {
    expect(selectMonitors(ms, undefined).rows.map((m) => m.name)).toEqual(["a alert", "e nodata", "d warn", "b ok", "c none"]);
  });

  it("keeps only the statuses asked for, and never a monitor with no status", () => {
    const r = selectMonitors(ms, { status: ["ALERT", "WARN", "NO_DATA"] });
    expect(r.rows.map((m) => m.name)).toEqual(["a alert", "e nodata", "d warn"]);
    expect(selectMonitors(ms, { status: ["OK"] }).rows.map((m) => m.name)).toEqual(["b ok"]);
  });

  it("cuts at the limit and counts the rest", () => {
    expect(selectMonitors(ms, undefined, 2)).toMatchObject({ more: 3 });
    expect(selectMonitors(ms, undefined, 2).rows).toHaveLength(2);
    const many = Array.from({ length: DEFAULT_SUMMARY_LIMIT + 3 }, (_, i) => mon(`m${i}`, "OK"));
    expect(selectMonitors(many, undefined)).toMatchObject({ more: 3 });
  });
});

describe("listParams", () => {
  it("sends the tag and name to the server and keeps the statuses here", () => {
    expect(listParams({ tag: "app:x", q: "queue", status: ["ALERT"] })).toEqual({ tag: "app:x", q: "queue" });
    expect(listParams(undefined)).toEqual({});
    expect(listParams({ tag: "", q: "" })).toEqual({});
  });
});

describe("serviceEnvStatus", () => {
  const g = (status: Status, tags: Record<string, string>): GroupState => ({
    group: Object.entries(tags).map(([k, v]) => `${k}:${v}`).join(",") || "*",
    tags,
    status,
    since: "",
    value: 1,
    last_eval: null,
    last_data: null,
    last_notified: null,
  });
  const byEnv = mon("5xx", "ALERT", { tags: ["service:api"] });
  const ungrouped = mon("oom", "OK", { tags: ["service:api"] });
  const states = new Map<number, GroupState[]>([
    [byEnv.id, [g("ALERT", { env: "staging" }), g("OK", { env: "prod" })]],
    [ungrouped.id, [g("OK", {})]],
  ]);

  it("does not turn a prod card red for a staging alert", () => {
    expect(serviceEnvStatus([byEnv, ungrouped], states, "api", "prod")).toBe("OK");
    expect(serviceEnvStatus([byEnv, ungrouped], states, "api", "staging")).toBe("ALERT");
  });

  it("lets an ungrouped monitor speak for every env, and a card with no env take every group", () => {
    const oomAlert = new Map(states).set(ungrouped.id, [g("ALERT", {})]);
    expect(serviceEnvStatus([byEnv, ungrouped], oomAlert, "api", "prod")).toBe("ALERT");
    expect(serviceEnvStatus([byEnv, ungrouped], states, "api", undefined)).toBe("ALERT");
  });

  it("says no monitors (undefined) and no group for this env yet (null) apart", () => {
    expect(serviceEnvStatus([byEnv], states, "worker", "prod")).toBeUndefined();
    expect(serviceEnvStatus([byEnv], states, "api", "dev")).toBeNull();
  });

  it("takes no claim from a monitor with no status (disabled, not running)", () => {
    const off = mon("off", null, { tags: ["service:api"] });
    expect(serviceEnvStatus([off], new Map([[off.id, [g("ALERT", {})]]]), "api", "prod")).toBeNull();
  });
});

describe("taggedMonitors", () => {
  it("keeps the monitors tagged with a shown service, up to the cap", () => {
    const ms = [mon("a", "OK", { tags: ["service:api"] }), mon("b", "OK", { tags: ["service:other"] }), mon("c", "OK", { tags: ["app:x"] }), mon("d", "OK", { tags: ["service:api"] })];
    expect(taggedMonitors(ms, new Set(["api"])).map((m) => m.name)).toEqual(["a", "d"]);
    expect(taggedMonitors(ms, new Set(["api"]), 1).map((m) => m.name)).toEqual(["a"]);
  });
});

describe("monitorByUid", () => {
  it("finds by uid and finds nothing for an empty or unknown one", () => {
    const ms = [mon("a", "OK", { uid: "a-uid" }), mon("b", "OK")];
    expect(monitorByUid(ms, "a-uid")?.name).toBe("a");
    expect(monitorByUid(ms, "nope")).toBeUndefined();
    expect(monitorByUid(ms, "")).toBeUndefined();
    expect(monitorByUid(ms, undefined)).toBeUndefined();
  });
});

describe("serviceCards", () => {
  it("puts the busiest first and keeps idle services, last", () => {
    const r = (service: string, requests: number, name = "http.request") =>
      ({ service, name, requests, requests_per_second: 0, errors: 0, error_pct: 0, p50_ms: null, p95_ms: null, p99_ms: null, sparkline: [] });
    const out = serviceCards([r("a-idle", 0), r("b-busy", 40), r("c-some", 7), r("b-busy", 40, "arq.job"), r("d-idle", 0)]);
    expect(out.map((x) => `${x.service}/${x.name}`)).toEqual(["b-busy/arq.job", "b-busy/http.request", "c-some/http.request", "a-idle/http.request", "d-idle/http.request"]);
  });
});
