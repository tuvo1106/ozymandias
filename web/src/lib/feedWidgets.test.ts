import { describe, expect, it } from "vitest";
import { DEFAULT_SUMMARY_LIMIT, monitorByUid, monitorsLink, selectMonitors, serviceEnvStatus } from "./feedWidgets";
import type { GroupStatus, Status, StoredMonitor } from "./monitorsApi";

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
    groups: status ? { [status]: 1 } : {},
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

  it("keeps a monitor with any group in a status asked for, as the Monitors page's ?status= does", () => {
    const mixed = mon("mixed", "ALERT", { groups: { ALERT: 1, WARN: 1, OK: 2 } });
    expect(selectMonitors([mixed], { status: ["WARN"] }).rows.map((m) => m.name)).toEqual(["mixed"]);
    expect(selectMonitors([mixed], { status: ["OK"] }).rows.map((m) => m.name)).toEqual(["mixed"]);
    expect(selectMonitors([mixed], { status: ["NO_DATA"] }).rows).toEqual([]);
  });

  it("cuts at the limit and counts the rest", () => {
    expect(selectMonitors(ms, undefined, 2)).toMatchObject({ more: 3 });
    expect(selectMonitors(ms, undefined, 2).rows).toHaveLength(2);
    const many = Array.from({ length: DEFAULT_SUMMARY_LIMIT + 3 }, (_, i) => mon(`m${i}`, "OK"));
    expect(selectMonitors(many, undefined)).toMatchObject({ more: 3 });
  });
});

describe("serviceEnvStatus", () => {
  const g = (status: Status, tags: Record<string, string>): GroupStatus => ({
    group: Object.entries(tags).map(([k, v]) => `${k}:${v}`).join(",") || "*",
    tags,
    status,
    value: 1,
  });
  const byEnv = mon("5xx", "ALERT", { tags: ["service:api"], group_states: [g("ALERT", { env: "staging" }), g("OK", { env: "prod" })] });
  const ungrouped = mon("oom", "OK", { tags: ["service:api"], group_states: [g("OK", {})] });

  it("does not turn a prod card red for a staging alert", () => {
    expect(serviceEnvStatus([byEnv, ungrouped], "api", "prod")).toBe("OK");
    expect(serviceEnvStatus([byEnv, ungrouped], "api", "staging")).toBe("ALERT");
  });

  it("reads a log monitor's @env the same way", () => {
    const logs = mon("worker errors", "ALERT", { tags: ["service:api"], group_states: [g("ALERT", { "@env": "staging" })] });
    expect(serviceEnvStatus([logs, ungrouped], "api", "prod")).toBe("OK");
    expect(serviceEnvStatus([logs], "api", "staging")).toBe("ALERT");
  });

  it("lets an ungrouped monitor speak for every env, and a card with no env take every group", () => {
    const oomAlert = { ...ungrouped, group_states: [g("ALERT", {})] };
    expect(serviceEnvStatus([byEnv, oomAlert], "api", "prod")).toBe("ALERT");
    expect(serviceEnvStatus([byEnv, ungrouped], "api", undefined)).toBe("ALERT");
  });

  it("says no monitors (undefined) and no group for this env (null) apart", () => {
    expect(serviceEnvStatus([byEnv], "worker", "prod")).toBeUndefined();
    expect(serviceEnvStatus([byEnv], "api", "dev")).toBeNull();
    // A disabled monitor comes with no group_states: tagged, but no claim.
    expect(serviceEnvStatus([mon("off", null, { tags: ["service:api"] })], "api", "prod")).toBeNull();
  });
});

describe("monitorsLink", () => {
  it("keeps the tag, the name and a single status", () => {
    expect(monitorsLink(undefined)).toBe("/monitors");
    const one = new URL(monitorsLink({ tag: "team:shop", q: "queue", status: ["ALERT"] }), "http://x");
    expect(Object.fromEntries(one.searchParams)).toEqual({ tag: "team:shop", q: "queue", status: "ALERT" });
    const set = new URL(monitorsLink({ tag: "team:shop", status: ["ALERT", "WARN"] }), "http://x");
    expect(Object.fromEntries(set.searchParams)).toEqual({ tag: "team:shop" });
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
