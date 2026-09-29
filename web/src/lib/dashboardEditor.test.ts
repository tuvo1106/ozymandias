import { describe, expect, it } from "vitest";
import type { Dashboard, Widget } from "./dashboard";
import {
  copyOf,
  definitionOf,
  editDashboard,
  exportDefinition,
  leadingAggregator,
  MAX_TITLE_BYTES,
  withSuffix,
  nextWidgetId,
  readEnum,
  readImport,
  readNumber,
  REDUCER_OPTIONS,
  rulesFor,
  sameDefinition,
  unknownKeys,
  unusedFields,
  WIDGET_TYPES,
  withQueryWidget,
} from "./dashboardEditor";

const chart: Widget = {
  id: "req",
  type: "timeseries",
  title: "req/s",
  layout: { x: 0, y: 0, w: 6, h: 3 },
  queries: [{ q: "sum:http.request.count{*}.as_rate()", display: "line" }],
  yaxis: { min: 0 },
};

const base: Dashboard = { title: "Checkout", widgets: [chart] };

describe("readEnum", () => {
  it("tells absent, known and unknown apart", () => {
    expect(readEnum(undefined, REDUCER_OPTIONS)).toEqual({ kind: "absent" });
    expect(readEnum("avg", REDUCER_OPTIONS)).toEqual({ kind: "known", value: "avg" });
    expect(readEnum("p42", REDUCER_OPTIONS)).toEqual({ kind: "unknown", raw: "p42" });
  });

  it("calls a present value of the wrong shape unknown, not absent — saving will send it", () => {
    expect(readEnum("", REDUCER_OPTIONS)).toEqual({ kind: "unknown", raw: "" });
    expect(readEnum(3, REDUCER_OPTIONS)).toEqual({ kind: "unknown", raw: "3" });
    expect(readEnum(null, REDUCER_OPTIONS)).toEqual({ kind: "unknown", raw: "null" });
  });
});

describe("readNumber", () => {
  it("keeps absent and zero apart", () => {
    expect(readNumber("")).toEqual({ kind: "absent" });
    expect(readNumber("  ")).toEqual({ kind: "absent" });
    expect(readNumber("0")).toEqual({ kind: "number", value: 0 });
  });

  it("does not write text that is not a number", () => {
    expect(readNumber("-")).toMatchObject({ kind: "invalid" });
    expect(readNumber("1e999")).toMatchObject({ kind: "invalid" });
    expect(readNumber("2.5", { integer: true })).toMatchObject({ kind: "invalid", reason: "a whole number" });
    expect(readNumber("11", { max: 10 })).toMatchObject({ kind: "invalid", reason: "at most 10" });
    expect(readNumber("-1", { min: 0 })).toMatchObject({ kind: "invalid", reason: "at least 0" });
  });
});

describe("TYPE_RULES", () => {
  it("has rules for every type, and none for a type this build does not know", () => {
    expect(WIDGET_TYPES).toEqual(["timeseries", "query_value", "toplist", "table", "heatmap", "note"]);
    expect(rulesFor("log_stream")).toBeUndefined();
    expect(rulesFor("toString")).toBeUndefined(); // not an inherited property
  });
});

describe("unusedFields", () => {
  it("is empty for a widget that uses everything it carries", () => {
    expect(unusedFields(chart)).toEqual([]);
  });

  it("lists what a type change leaves behind", () => {
    const flipped: Widget = { ...chart, type: "toplist", conditional_formats: [{ op: ">", value: 1, color: "red" }] };
    expect(unusedFields(flipped).map((f) => [f.field, f.query])).toEqual([
      ["yaxis", undefined],
      ["conditional_formats", undefined],
      ["display", 0],
    ]);
  });

  it("counts zero as present where the server does, and not where it does not", () => {
    expect(unusedFields({ ...chart, precision: 0 }).map((f) => f.field)).toEqual(["precision"]);
    // limit 0 is the server's "not set"; markdown "" is too.
    expect(unusedFields({ ...chart, limit: 0, markdown: "" })).toEqual([]);
  });

  it("flags queries on a note and reducers on a chart", () => {
    const note: Widget = { id: "n", type: "note", layout: chart.layout, markdown: "x", queries: [{ q: "a" }] };
    expect(unusedFields(note).map((f) => f.field)).toEqual(["queries"]);
    const reduced: Widget = { ...chart, queries: [{ q: "a", reducer: "avg" }] };
    expect(unusedFields(reduced)).toEqual([expect.objectContaining({ field: "reducer", query: 0 })]);
  });

  it("says nothing about a type it does not know the rules of", () => {
    expect(unusedFields({ ...chart, type: "log_stream" as Widget["type"], limit: 5 })).toEqual([]);
  });
});

describe("unknownKeys", () => {
  it("names keys this build does not read", () => {
    expect(unknownKeys({ ...chart, links: [] }, "widget")).toEqual(["links"]);
    expect(unknownKeys({ q: "x", alias: "y" }, "query")).toEqual(["alias"]);
    expect(unknownKeys(base, "dashboard")).toEqual([]);
  });
});

describe("editDashboard", () => {
  it("adds a widget below everything with nothing chosen for the author", () => {
    const d = editDashboard(base, { type: "addWidget", widgetType: "toplist" });
    const added = d.widgets[1] as Widget;
    expect(added).toEqual({
      id: "w2",
      type: "toplist",
      title: "New toplist",
      layout: { x: 0, y: 3, w: 4, h: 3 },
      queries: [{ q: "" }],
    });
    // No reducer: which number a toplist ranks by is the author's question.
    expect(added.queries?.[0]?.reducer).toBeUndefined();
    const note = editDashboard(base, { type: "addWidget", widgetType: "note" }).widgets[1];
    expect(note).toMatchObject({ markdown: "" });
    expect(note).not.toHaveProperty("queries");
  });

  it("changes only the type, keeping what the new type does not use", () => {
    const d = editDashboard(base, { type: "setType", id: "req", to: "query_value" });
    expect(d.widgets[0]).toEqual({ ...chart, type: "query_value" });
  });

  it("gives a type change to a note its markdown, and back its query", () => {
    const note = editDashboard(base, { type: "setType", id: "req", to: "note" });
    expect(note.widgets[0]?.markdown).toBe("");
    const bare: Dashboard = { title: "t", widgets: [{ id: "n", type: "note", layout: chart.layout, markdown: "m" }] };
    expect(editDashboard(bare, { type: "setType", id: "n", to: "table" }).widgets[0]?.queries).toEqual([{ q: "" }]);
  });

  it("removes a field by deleting its key, not by writing undefined into it", () => {
    const d = editDashboard(base, { type: "setWidget", id: "req", patch: { yaxis: undefined } });
    expect(Object.keys(d.widgets[0] as Widget)).not.toContain("yaxis");
    const q = editDashboard(base, { type: "setQuery", id: "req", index: 0, patch: { display: undefined } });
    expect(Object.keys(q.widgets[0]?.queries?.[0] ?? {})).toEqual(["q"]);
  });

  it("removes an unused per-query field from the right query", () => {
    const two: Dashboard = {
      title: "t",
      widgets: [{ ...chart, type: "table", queries: [{ q: "a", reducer: "avg" }, { q: "b", display: "bars", reducer: "sum" }] }],
    };
    const field = unusedFields(two.widgets[0] as Widget).find((f) => f.field === "display");
    expect(field).toMatchObject({ field: "display", query: 1 });
    const d = editDashboard(two, { type: "removeUnused", id: "req", field: field! });
    expect(d.widgets[0]?.queries).toEqual([{ q: "a", reducer: "avg" }, { q: "b", reducer: "sum" }]);
  });

  it("keeps keys this build does not know through every edit", () => {
    const odd = { ...base, owner: "sre", widgets: [{ ...chart, links: ["x"] }] } as unknown as Dashboard;
    let d = editDashboard(odd, { type: "setWidget", id: "req", patch: { title: "renamed" } });
    d = editDashboard(d, { type: "setLayout", id: "req", layout: { x: 3, y: 0, w: 6, h: 3 }, settle: true });
    d = editDashboard(d, { type: "setMeta", patch: { title: "T" } });
    expect(d).toMatchObject({ owner: "sre", widgets: [{ links: ["x"], title: "renamed" }] });
  });

  it("clamps a dragged layout and pushes what it lands on down when it settles", () => {
    const two: Dashboard = { title: "t", widgets: [chart, { ...chart, id: "b", layout: { x: 6, y: 0, w: 6, h: 3 } }] };
    const dragging = editDashboard(two, { type: "setLayout", id: "b", layout: { x: 4, y: 0, w: 6, h: 3 }, settle: false });
    expect(dragging.widgets[0]?.layout).toEqual(chart.layout); // not yet
    const dropped = editDashboard(two, { type: "setLayout", id: "b", layout: { x: 4, y: 0, w: 6, h: 3 }, settle: true });
    expect(dropped.widgets.map((w) => w.layout)).toEqual([
      { x: 0, y: 3, w: 6, h: 3 },
      { x: 4, y: 0, w: 6, h: 3 },
    ]);
    const offGrid = editDashboard(two, { type: "setLayout", id: "b", layout: { x: 11, y: -2, w: 6, h: 3 }, settle: false });
    expect(offGrid.widgets[1]?.layout).toEqual({ x: 6, y: 0, w: 6, h: 3 });
  });

  it("duplicates a widget under a new id, deep-copied", () => {
    const d = editDashboard(base, { type: "duplicateWidget", id: "req" });
    const copy = d.widgets[1] as Widget;
    expect(copy.id).toBe("w2");
    expect(copy.layout).toEqual({ x: 0, y: 3, w: 6, h: 3 });
    copy.queries![0]!.q = "changed";
    expect(chart.queries?.[0]?.q).not.toBe("changed");
  });

  it("drops a conditional_formats list that becomes empty rather than saving []", () => {
    let d = editDashboard(base, { type: "setType", id: "req", to: "query_value" });
    d = editDashboard(d, { type: "addFormat", id: "req" });
    expect(d.widgets[0]?.conditional_formats).toEqual([{ op: ">", value: 0, color: "red" }]);
    d = editDashboard(d, { type: "setFormat", id: "req", index: 0, patch: { color: "yellow" } });
    expect(d.widgets[0]?.conditional_formats?.[0]?.color).toBe("yellow");
    d = editDashboard(d, { type: "removeFormat", id: "req", index: 0 });
    expect(d.widgets[0]).not.toHaveProperty("conditional_formats");
  });

  it("ignores an edit naming a widget that is gone", () => {
    expect(editDashboard(base, { type: "setWidget", id: "nope", patch: { title: "x" } })).toEqual(base);
    expect(editDashboard(base, { type: "duplicateWidget", id: "nope" })).toBe(base);
  });

  it("adds and removes queries by position", () => {
    let d = editDashboard(base, { type: "addQuery", id: "req" });
    expect(d.widgets[0]?.queries).toHaveLength(2);
    d = editDashboard(d, { type: "removeQuery", id: "req", index: 0 });
    expect(d.widgets[0]?.queries).toEqual([{ q: "" }]);
  });
});

describe("nextWidgetId", () => {
  it("never reuses an id", () => {
    expect(nextWidgetId([{ ...chart, id: "w2" }])).toBe("w3");
    expect(nextWidgetId([])).toBe("w1");
  });
});

describe("definitions, copies and exports", () => {
  const stored = { ...base, uid: "checkout", id: 4, provisioned: false, created_at: "t", updated_at: "t" };

  it("strips the database's columns, and says which", () => {
    const { definition, dropped } = definitionOf(stored);
    expect(dropped).toEqual(["id", "provisioned", "created_at", "updated_at"]);
    expect(definition).toEqual({ ...base, uid: "checkout" });
  });

  it("exports the definition alone", () => {
    expect(JSON.parse(exportDefinition(stored))).toEqual({ ...base, uid: "checkout" });
  });

  it("copies without the uid, which is the original's identity", () => {
    expect(copyOf(stored)).toEqual({ ...base, title: "Checkout (copy)" });
  });

  // A template instance's title is already cut to the limit by the server;
  // appending alone would make a copy whose first save is refused.
  it("keeps a copy's title within the server's limit, in bytes", () => {
    const long = copyOf({ ...base, title: "é".repeat(150) }).title;
    expect(new TextEncoder().encode(long).length).toBeLessThanOrEqual(MAX_TITLE_BYTES);
    expect(long.endsWith(" (copy)")).toBe(true);
    expect(long).not.toContain("\uFFFD");
  });

  // The server cut the template's part so the service would survive; the
  // service is what tells two instances apart.
  it("cuts an instance's title before the service, not through it", () => {
    const at = (service: string) => copyOf({ ...base, title: `${"t".repeat(200 - 2 - service.length)}: ${service}` }, service).title;
    for (const service of ["checkout", "payments"]) {
      const title = at(service);
      expect(title.endsWith(`: ${service} (copy)`)).toBe(true);
      expect(new TextEncoder().encode(title).length).toBe(MAX_TITLE_BYTES);
    }
    // Not an instance title after all: cut like any other.
    expect(copyOf({ ...base, title: "x".repeat(200) }, "checkout").title).toBe(`${"x".repeat(193)} (copy)`);
  });

  it("cuts on a character, never inside one", () => {
    expect(withSuffix("aé", "!", 3)).toBe("a!"); // é would need bytes 2–3
    expect(withSuffix("abc", "!", 10)).toBe("abc!");
  });
});

describe("withQueryWidget", () => {
  const d: Dashboard = { title: "D", widgets: [chart, { ...chart, id: "w2", layout: { x: 6, y: 2, w: 6, h: 4 } }] };

  it("adds a timeseries of the query below everything, with a new id", () => {
    const { dashboard, id } = withQueryWidget(d, "sum:m{*}");
    expect(dashboard.widgets.slice(0, 2)).toEqual(d.widgets);
    const added = dashboard.widgets[2]!;
    expect(added).toMatchObject({ id, type: "timeseries", title: "sum:m{*}", queries: [{ q: "sum:m{*}" }] });
    expect(d.widgets.map((w) => w.id)).not.toContain(id);
    expect(added.layout.y).toBe(6);
    expect(d.widgets).toHaveLength(2);
  });

  // The title is the query until the author names it, and the server
  // refuses a title over the limit.
  it("cuts a long query to the title limit, in bytes", () => {
    const title = withQueryWidget(d, `sum:${"é".repeat(150)}{*}`).dashboard.widgets[2]!.title!;
    expect(new TextEncoder().encode(title).length).toBeLessThanOrEqual(MAX_TITLE_BYTES);
    expect(title.startsWith("sum:é")).toBe(true);
  });
});

describe("readImport", () => {
  it("tells every way of not being a definition apart", () => {
    expect(readImport("  ")).toEqual({ kind: "empty" });
    expect(readImport("{")).toMatchObject({ kind: "notJson" });
    expect(readImport("[]")).toEqual({ kind: "notDashboard", problems: ["It is not a JSON object."] });
  });

  it("names every problem, not the first", () => {
    const r = readImport(JSON.stringify({ widgets: [{ id: 1, layout: { x: 0, y: 0, w: "6", h: 1 } }, "w"] }));
    expect(r).toEqual({
      kind: "notDashboard",
      problems: [
        "title is missing or not a string.",
        "widgets[0]: id is missing or not a string.",
        "widgets[0]: type is missing or not a string.",
        "widgets[0]: layout needs numeric x, y, w and h.",
        "widgets[1] is not an object.",
      ],
    });
  });

  it("accepts a stored dashboard pasted from GET, dropping the database's columns", () => {
    const r = readImport(JSON.stringify({ ...base, id: 3, provisioned: true, created_at: "a", updated_at: "b" }));
    expect(r).toEqual({ kind: "ok", dashboard: base, dropped: ["id", "provisioned", "created_at", "updated_at"] });
  });

  // Everything the editor calls a string method on has to be a string, or an
  // "ok" import is a TypeError on the next render — the draft gone with it.
  it("refuses shapes the editor would crash on, naming each", () => {
    const r = readImport(
      JSON.stringify({
        title: "t",
        description: 3,
        template_vars: [{ tag: "env" }, null, { name: "a", tag: "b", default: 1 }],
        widgets: [{ id: "w", type: "note", layout: { x: 0, y: 0, w: 1, h: 1 }, markdown: 5, title: [], yaxis: 1, conditional_formats: [2] }],
      }),
    );
    expect(r).toEqual({
      kind: "notDashboard",
      problems: [
        "widgets[0]: title is not a string.",
        "widgets[0]: markdown is not a string.",
        "widgets[0]: yaxis is not an object.",
        "widgets[0]: conditional_formats must be a list of objects.",
        "description is not a string.",
        "template_vars[0]: name is missing or not a string.",
        "template_vars[1] is not an object.",
        "template_vars[2]: default is not a string.",
      ],
    });
  });

  it("leaves what the server judges to the server", () => {
    const odd = { title: "t", widgets: [{ ...chart, type: "log_stream", queries: [{ q: "not a query" }] }] };
    expect(readImport(JSON.stringify(odd))).toMatchObject({ kind: "ok", dropped: [] });
  });
});

describe("helpers", () => {
  it("reads a leading aggregator, and nothing from an expression", () => {
    expect(leadingAggregator(" DIST:lat{*}")).toBe("dist");
    expect(leadingAggregator("abs(sum:x{*})")).toBeUndefined();
    expect(leadingAggregator("")).toBeUndefined();
  });

  it("compares definitions by content", () => {
    expect(sameDefinition(base, structuredClone(base))).toBe(true);
    expect(sameDefinition(base, { ...base, title: "x" })).toBe(false);
  });

  // Clearing a field deletes its key; typing the same value back re-adds it
  // at the end. That is the same definition, not an unsaved change.
  it("ignores key order, but not array order", () => {
    let d = editDashboard(base, { type: "setWidget", id: "req", patch: { title: undefined } });
    d = editDashboard(d, { type: "setWidget", id: "req", patch: { title: "req/s" } });
    expect(Object.keys(d.widgets[0] as Widget).at(-1)).toBe("title");
    expect(sameDefinition(base, d)).toBe(true);
    const two = { ...base, widgets: [chart, { ...chart, id: "b" }] };
    expect(sameDefinition(two, { ...two, widgets: [...two.widgets].reverse() })).toBe(false);
  });
});
