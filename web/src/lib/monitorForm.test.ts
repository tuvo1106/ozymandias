import { describe, expect, it } from "vitest";
import { definitionOf, emptyForm, formOf, previewDefinitionOf, splitTags } from "./monitorForm";
import type { MonitorDefinition } from "./monitorsApi";

describe("definitionOf", () => {
  // An untouched box is the server's default, not 0.
  it("sends only the options that were filled in", () => {
    const r = definitionOf({ ...emptyForm(), name: " Backlog ", renotify_interval_m: "60" });
    expect(r).toEqual({
      ok: true,
      definition: {
        name: "Backlog",
        type: "metric",
        query: emptyForm().query,
        options: { thresholds: {}, renotify_interval_m: 60 },
        message: emptyForm().message,
        tags: [],
        enabled: true,
      },
    });
  });

  it("keeps a 0 that was typed", () => {
    const r = definitionOf({ ...emptyForm(), name: "x", evaluation_delay_s: "0", warning: "0" });
    expect(r.ok && r.definition.options).toEqual({ thresholds: { warning: 0 }, evaluation_delay_s: 0 });
  });

  it("names every problem", () => {
    const r = definitionOf({ ...emptyForm(), name: "", query: " ", warning: "ten", renotify_interval_m: "1.5" });
    expect(r).toEqual({
      ok: false,
      errors: ["name is required", "query is required", "warning must be a number", "renotify interval must be a whole number"],
    });
  });

  it("sends a uid only when one is given", () => {
    const r = definitionOf({ ...emptyForm(), name: "x", uid: " jobs " });
    expect(r.ok && r.definition.uid).toBe("jobs");
    const r2 = definitionOf({ ...emptyForm(), name: "x" });
    expect(r2.ok && "uid" in r2.definition).toBe(false);
  });
});

describe("formOf", () => {
  // A stored monitor opened and saved untouched sends what it was stored as.
  it("round-trips a definition", () => {
    const def: MonitorDefinition = {
      uid: "jobs",
      name: "Backlog",
      type: "log",
      query: 'logs("status:error").rollup("count").last("5m") > 5',
      options: {
        thresholds: { warning: 2, critical_recovery: 4, warning_recovery: 1 },
        evaluation_interval_s: 60,
        evaluation_delay_s: 0,
        require_full_window: true,
        notify_no_data: true,
        no_data_timeframe_m: 10,
        renotify_interval_m: 30,
        min_consecutive_failures: 2,
      },
      message: "@hook",
      tags: ["team:a", "env:prod"],
      enabled: false,
    };
    expect(definitionOf(formOf(def))).toEqual({ ok: true, definition: def });
  });
});

describe("previewDefinitionOf", () => {
  it("previews a monitor that has no name yet, and still names bad boxes", () => {
    const r = previewDefinitionOf({ ...emptyForm(), name: "  " });
    expect(r.ok && r.definition.name).toBe("Untitled monitor");
    expect(previewDefinitionOf({ ...emptyForm(), warning: "x" })).toEqual({ ok: false, errors: ["warning must be a number"] });
  });
});

describe("splitTags", () => {
  it("splits on commas and spaces and drops repeats", () => {
    expect(splitTags(" a:1, b:2  a:1,,c ")).toEqual(["a:1", "b:2", "c"]);
  });
});
