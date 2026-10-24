import { describe, expect, it } from "vitest";
import { DEFAULT_SUMMARY_LIMIT, listParams, monitorByUid, selectMonitors, serviceCards, serviceStatuses } from "./feedWidgets";
import type { Status, StoredMonitor } from "./monitorsApi";

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

describe("serviceStatuses", () => {
  it("takes each service's worst status from the monitors tagged with it", () => {
    const s = serviceStatuses([
      mon("1", "OK", { tags: ["service:api", "app:x"] }),
      mon("2", "ALERT", { tags: ["service:api"] }),
      mon("3", "WARN", { tags: ["service:worker"] }),
      mon("4", "ALERT", { tags: ["app:x"] }),
    ]);
    expect(s.get("api")).toBe("ALERT");
    expect(s.get("worker")).toBe("WARN");
    expect(s.size).toBe(2);
  });

  it("knows a service whose monitors have no status, and is not fooled by order", () => {
    expect(serviceStatuses([mon("1", null, { tags: ["service:api"] })]).get("api")).toBeNull();
    expect(serviceStatuses([mon("1", null, { tags: ["service:api"] })]).has("api")).toBe(true);
    const later = serviceStatuses([mon("1", "WARN", { tags: ["service:api"] }), mon("2", null, { tags: ["service:api"] })]);
    expect(later.get("api")).toBe("WARN");
  });

  it("has no entry for a service with no monitors: absent, not OK", () => {
    expect(serviceStatuses([mon("1", "OK", { tags: ["app:x"] })]).has("api")).toBe(false);
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
