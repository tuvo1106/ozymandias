import { describe, expect, it } from "vitest";
import type { Widget } from "./dashboard";
import { MAX_QUERIES_PER_BATCH } from "./dashboardsApi";
import {
  collectRequests,
  mergeWidgetResults,
  pairResults,
  requestsKey,
  widgetResults,
  widgetSketch,
  sharedWarnings,
  type QuerySlot,
} from "./dashboardQueries";

function chart(id: string, ...queries: string[]): Widget {
  return { id, type: "timeseries", layout: { x: 0, y: 0, w: 6, h: 3 }, queries: queries.map((q) => ({ q })) };
}

describe("collectRequests", () => {
  it("flattens every widget's queries in order", () => {
    const { chunks } = collectRequests([chart("a", "sum:x{*}", "sum:y{*}"), chart("b", "avg:z{*}")]);
    expect(chunks).toHaveLength(1);
    expect(chunks[0]).toEqual([
      { widgetId: "a", queryIndex: 0, q: "sum:x{*}" },
      { widgetId: "a", queryIndex: 1, q: "sum:y{*}" },
      { widgetId: "b", queryIndex: 0, q: "avg:z{*}" },
    ]);
  });

  // A heatmap draws a distribution, which /api/v1/query refuses outright — the
  // two endpoints reject each other's queries on purpose (ADR-0019). Putting
  // one in the batch would make the widget a 400 every single draw.
  it("keeps heatmaps out of the batch", () => {
    const heatmap: Widget = {
      id: "h",
      type: "heatmap",
      layout: { x: 0, y: 0, w: 6, h: 3 },
      queries: [{ q: "dist:lat{*}" }],
    };
    const { chunks, heatmaps } = collectRequests([chart("a", "sum:x{*}"), heatmap]);
    expect(chunks[0]?.map((s) => s.q)).toEqual(["sum:x{*}"]);
    expect(heatmaps).toEqual([{ widgetId: "h", q: "dist:lat{*}" }]);
  });

  it("asks for nothing on behalf of a note or a blank query", () => {
    const note: Widget = { id: "n", type: "note", layout: { x: 0, y: 0, w: 12, h: 1 }, markdown: "hi" };
    const { chunks, heatmaps } = collectRequests([note, chart("a", "   ")]);
    expect(chunks).toEqual([]);
    expect(heatmaps).toEqual([]);
  });

  // A definition may hold 100 widgets of 10 queries; a batch takes 50. Sending
  // them all would be one request-level 400 and a blank dashboard.
  it("splits into batches the server will accept", () => {
    const widgets = Array.from({ length: 12 }, (_, i) => chart(`w${i}`, ...Array.from({ length: 10 }, (_, j) => `sum:m${i}_${j}{*}`)));
    const { chunks } = collectRequests(widgets);
    expect(chunks.reduce((n, c) => n + c.length, 0)).toBe(120);
    expect(chunks).toHaveLength(Math.ceil(120 / MAX_QUERIES_PER_BATCH));
    for (const chunk of chunks) expect(chunk.length).toBeLessThanOrEqual(MAX_QUERIES_PER_BATCH);
  });

  // Each chunk is indexed from zero, because the server numbers results by
  // position within the request it was given.
  it("restarts the index in every chunk", () => {
    const widgets = Array.from({ length: 60 }, (_, i) => chart(`w${i}`, `sum:m${i}{*}`));
    const { chunks } = collectRequests(widgets);
    expect(chunks[1]?.[0]).toEqual({ widgetId: "w50", queryIndex: 0, q: "sum:m50{*}" });
  });
});

describe("requestsKey", () => {
  it("changes when a query does and not when it does not", () => {
    const a = requestsKey(collectRequests([chart("a", "sum:x{*}")]));
    const same = requestsKey(collectRequests([chart("a", "sum:x{*}")]));
    const edited = requestsKey(collectRequests([chart("a", "sum:y{*}")]));
    expect(a).toBe(same);
    expect(a).not.toBe(edited);
  });

  // Moving a widget is not a reason to re-query it.
  it("ignores where a widget sits", () => {
    const here = chart("a", "sum:x{*}");
    const there: Widget = { ...here, layout: { x: 6, y: 9, w: 3, h: 1 } };
    expect(requestsKey(collectRequests([here]))).toBe(requestsKey(collectRequests([there])));
  });
});

describe("pairResults", () => {
  const slots: QuerySlot[] = [
    { widgetId: "a", queryIndex: 0, q: "q0" },
    { widgetId: "b", queryIndex: 0, q: "q1" },
    { widgetId: "a", queryIndex: 1, q: "q2" },
  ];

  it("routes each result to the widget that asked for it", () => {
    const byWidget = pairResults(slots, [{ index: 0 }, { index: 1 }, { index: 2 }]);
    expect(byWidget.get("a")?.get(0)).toEqual({ asked: "q0", result: { index: 0 } });
    expect(byWidget.get("a")?.get(1)).toEqual({ asked: "q2", result: { index: 2 } });
    expect(byWidget.get("b")?.get(0)).toEqual({ asked: "q1", result: { index: 1 } });
  });

  // The worst failure a dashboard has is showing one widget another widget's
  // numbers, so the result's own index decides — never its position.
  it("uses the reported index, not the array position", () => {
    const byWidget = pairResults(slots, [{ index: 2 }, { index: 0 }]);
    expect(byWidget.get("a")?.get(1)?.result).toEqual({ index: 2 });
    expect(byWidget.get("a")?.get(0)?.result).toEqual({ index: 0 });
    expect(byWidget.get("b")).toBeUndefined();
  });

  it("drops an index that belongs to no slot", () => {
    expect(pairResults(slots, [{ index: 99 }, { index: -1 }]).size).toBe(0);
  });
});

describe("mergeWidgetResults", () => {
  it("combines chunks without losing a widget split across them", () => {
    const first = new Map([["a", new Map([[0, "x"]])]]);
    const second = new Map([
      ["a", new Map([[1, "y"]])],
      ["b", new Map([[0, "z"]])],
    ]);
    const merged = mergeWidgetResults([first, second]);
    expect([...(merged.get("a") ?? [])]).toEqual([
      [0, "x"],
      [1, "y"],
    ]);
    expect(merged.get("b")?.get(0)).toBe("z");
  });

  it("does not write into the maps it was given", () => {
    const first = new Map([["a", new Map([[0, "x"]])]]);
    mergeWidgetResults([first, new Map([["a", new Map([[1, "y"]])]])]);
    expect(first.get("a")?.size).toBe(1);
  });
});

describe("widgetResults", () => {
  it("is in the definition's order with a hole where an answer is missing", () => {
    const w = chart("a", "q0", "q1", "q2");
    const byWidget = new Map([["a", new Map([[2, { asked: "q2", result: "third" }]])]]);
    expect(widgetResults(w, byWidget)).toEqual([undefined, undefined, "third"]);
  });

  // The editor deletes the first of two queries while the answer to the old
  // pair is still on screen. By position, the old first answer would now be
  // drawn under the name of what used to be the second query.
  it("does not hand a query an answer to different text", () => {
    const before = chart("a", "q0", "q1");
    const byWidget = pairResults(
      [
        { widgetId: "a", queryIndex: 0, q: "q0" },
        { widgetId: "a", queryIndex: 1, q: "q1" },
      ],
      [{ index: 0 }, { index: 1 }],
    );
    expect(widgetResults(before, byWidget)).toEqual([{ index: 0 }, { index: 1 }]);
    const after = chart("a", "q1");
    expect(widgetResults(after, byWidget)).toEqual([undefined]);
  });

  it("compares against the text as sent, which is trimmed", () => {
    const byWidget = new Map([["a", new Map([[0, { asked: "q0", result: "r" }]])]]);
    expect(widgetResults(chart("a", "  q0 "), byWidget)).toEqual(["r"]);
  });

  it("is empty for a widget with no queries", () => {
    const note: Widget = { id: "n", type: "note", layout: { x: 0, y: 0, w: 1, h: 1 } };
    expect(widgetResults(note, new Map())).toEqual([]);
  });
});

describe("widgetSketch", () => {
  it("hands a heatmap only a sketch of one of its queries as written now", () => {
    const sketches = new Map([["h", { asked: "dist:a{*}" }]]);
    expect(widgetSketch(chart("h", "dist:a{*}"), sketches)).toEqual({ asked: "dist:a{*}" });
    expect(widgetSketch(chart("h", "dist:b{*}"), sketches)).toBeUndefined();
    expect(widgetSketch(chart("x", "dist:a{*}"), sketches)).toBeUndefined();
  });
});

describe("sharedWarnings", () => {
  const ok = (index: number, warnings: string[]) => ({ index, status: "ok", warnings });
  const refused = (index: number) => ({ index, status: "error", warnings: [] as string[] });
  const answer = (q: string, result: ReturnType<typeof ok>) => ({ asked: q, result });
  const env = "$env resolved to no filter";
  const note: Widget = { id: "n", type: "note", layout: { x: 0, y: 0, w: 1, h: 1 }, markdown: "m" };
  const heat: Widget = { ...chart("h", "dist:a{*}"), type: "heatmap" };

  it("hoists what every answering widget says, including a heatmap", () => {
    const got = sharedWarnings(
      [chart("a", "qa"), chart("b", "qb"), heat, note],
      new Map([
        ["a", new Map([[0, answer("qa", ok(0, [env, "cap hit"]))]])],
        ["b", new Map([[0, answer("qb", ok(1, [env]))]])],
      ]),
      new Map([["h", { asked: "dist:a{*}", data: { warnings: [env] } }]]),
    );
    expect(got).toEqual([env]);
  });

  it("hoists nothing while any widget is still waiting", () => {
    const byWidget = new Map([["a", new Map([[0, answer("qa", ok(0, [env]))]])], ["b", new Map([[0, answer("qb", ok(1, [env]))]])]]);
    expect(sharedWarnings([chart("a", "qa"), chart("b", "qb"), heat], byWidget, new Map())).toEqual([]);
    // Waiting includes an answer to text the widget no longer says.
    expect(sharedWarnings([chart("a", "qa"), chart("b", "qb2")], byWidget, new Map())).toEqual([]);
    // And one query of two not back yet.
    expect(sharedWarnings([chart("a", "qa", "qa2"), chart("b", "qb")], byWidget, new Map())).toEqual([]);
  });

  it("leaves a refused widget out rather than reading its silence as disagreement", () => {
    const got = sharedWarnings(
      [chart("a", "qa"), chart("b", "qb"), chart("c", "qc")],
      new Map([
        ["a", new Map([[0, answer("qa", ok(0, [env]))]])],
        ["b", new Map([[0, answer("qb", ok(1, [env]))]])],
        ["c", new Map([[0, answer("qc", refused(2))]])],
      ]),
      new Map(),
    );
    expect(got).toEqual([env]);
  });

  it("does not hoist one widget's own warning, or one some widget lacks", () => {
    const single = new Map([["a", new Map([[0, answer("qa", ok(0, [env]))]])]]);
    expect(sharedWarnings([chart("a", "qa"), note], single, new Map())).toEqual([]);
    const split = new Map([
      ["a", new Map([[0, answer("qa", ok(0, [env]))]])],
      ["b", new Map([[0, answer("qb", ok(1, []))]])],
    ]);
    expect(sharedWarnings([chart("a", "qa"), chart("b", "qb")], split, new Map())).toEqual([]);
  });

  it("skips a widget whose every query is blank: nothing was asked, so nothing is coming", () => {
    const byWidget = new Map([["a", new Map([[0, answer("qa", ok(0, [env]))]])], ["b", new Map([[0, answer("qb", ok(1, [env]))]])]]);
    expect(sharedWarnings([chart("a", "qa"), chart("b", "qb"), chart("c", " ")], byWidget, new Map())).toEqual([env]);
  });
});
