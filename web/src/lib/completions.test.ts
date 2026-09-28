import { describe, expect, it } from "vitest";
import { applyCompletion, localCompletions, storeCompletions, VOCABULARY } from "./completions";
import { completionContext, type CompletionContext } from "./queryContext";

function ctxAt(marked: string): CompletionContext {
  return completionContext(marked.replace("|", ""), marked.indexOf("|"));
}

const labels = (l: ReturnType<typeof storeCompletions> | undefined) =>
  l?.status === "ready" ? l.items.map((i) => i.label) : l;

describe("localCompletions", () => {
  it("offers aggregators and functions at the start of a term", () => {
    expect(labels(localCompletions(ctxAt("p9|"), []))).toEqual(["p90", "p95", "p99"]);
    expect(labels(localCompletions(ctxAt("ti|"), []))).toEqual(["timeshift"]);
    // Aggregators are case-insensitive in the grammar, so matching is too.
    expect(labels(localCompletions(ctxAt("SU|"), []))).toEqual(["sum"]);
  });

  it("offers every word the Go vocabulary lists", () => {
    const all = localCompletions(ctxAt("|"), []);
    expect(all?.status === "ready" && all.items.length).toBe(VOCABULARY.aggregators.length + VOCABULARY.functions.length);
  });

  it("offers modifiers, and each modifier's own arguments", () => {
    expect(localCompletions(ctxAt("sum:m{*}.as|"), [])).toMatchObject({
      status: "ready",
      items: [
        { label: "as_count", insert: "as_count()" },
        { label: "as_rate", insert: "as_rate()" },
      ],
    });
    expect(labels(localCompletions(ctxAt("sum:m{*}.fill(|"), []))).toEqual(VOCABULARY.fill_modes);
    expect(labels(localCompletions(ctxAt("sum:m{*}.rollup(l|"), []))).toEqual(["last"]);
  });

  it("offers nothing — not an empty list — for an argument it does not know", () => {
    expect(localCompletions(ctxAt("sum:m{*}.nosuch(|"), [])).toEqual({ status: "none" });
  });

  it("offers the dashboard's variables, and says when it has none", () => {
    expect(labels(localCompletions(ctxAt("sum:m{$|"), ["env", "service"]))).toEqual(["$env", "$service"]);
    expect(localCompletions(ctxAt("sum:m{$|"), [])).toMatchObject({ status: "empty", message: expect.stringMatching(/declares no template variables/) });
  });

  it("says a prefix matched nothing only as that", () => {
    expect(localCompletions(ctxAt("zz|"), [])).toEqual({ status: "empty", message: 'No aggregator or function starts with "zz".' });
  });

  it("leaves store positions to the store", () => {
    expect(localCompletions(ctxAt("sum:|"), [])).toBeUndefined();
    expect(localCompletions(ctxAt("sum:m{|"), [])).toBeUndefined();
  });
});

describe("storeCompletions", () => {
  it("distinguishes loading, failed, blocked, empty and ready", () => {
    const metric = ctxAt("sum:ht|");
    expect(storeCompletions(metric, { loading: true })).toEqual({ status: "loading" });
    expect(storeCompletions(metric, { loading: false, error: new Error("down") })).toEqual({
      status: "failed",
      message: "Suggestions are unavailable: down",
    });
    expect(storeCompletions(metric, { loading: false, data: ["cpu"] })).toEqual({
      status: "empty",
      message: 'No metric starts with "ht".',
    });
    expect(storeCompletions(metric, { loading: false, data: ["http.a", "cpu"] })).toMatchObject({
      status: "ready",
      items: [{ label: "http.a", insert: "http.a{" }],
    });
  });

  it("does not claim an empty store from an empty prefix's answer", () => {
    expect(storeCompletions(ctxAt("sum:|"), { loading: false, data: [] })).toEqual({
      status: "empty",
      message: "The store holds no metrics yet.",
    });
  });

  it("needs a metric before it can suggest tags", () => {
    expect(storeCompletions(ctxAt("sum:{|"), { loading: false, data: ["a"] })).toMatchObject({ status: "blocked" });
  });

  it("says a value list is only the values the store returned", () => {
    expect(storeCompletions(ctxAt("sum:m{env:q|"), { loading: false, data: ["prod"] })).toMatchObject({
      status: "empty",
      message: expect.stringMatching(/among those the store returned/),
    });
  });

  it("completes keys with their colon, group keys without", () => {
    expect(storeCompletions(ctxAt("sum:m{se|"), { loading: false, data: ["service"] })).toMatchObject({
      items: [{ insert: "service:" }],
    });
    expect(storeCompletions(ctxAt("sum:m{*} by {se|"), { loading: false, data: ["service"] })).toMatchObject({
      items: [{ insert: "service" }],
    });
  });
});

describe("applyCompletion", () => {
  const apply = (marked: string, insert: string) => {
    const ctx = ctxAt(marked);
    if (ctx.kind === "none") throw new Error("no context");
    return applyCompletion(marked.replace("|", ""), ctx, { label: insert, insert });
  };

  it("replaces the word at the caret, including the part after it", () => {
    expect(apply("sum:ht|tp.x{*}", "http.request.count{")).toEqual({ text: "sum:http.request.count{*}", caret: 23 });
  });

  it("does not double punctuation that is already there", () => {
    expect(apply("su|:m{*}", "sum:").text).toBe("sum:m{*}");
    expect(apply("sum:m{*}.as_r|()", "as_rate()").text).toBe("sum:m{*}.as_rate()");
  });

  it("puts the caret after what it inserted", () => {
    const r = apply("s|", "sum:");
    expect(r).toEqual({ text: "sum:", caret: 4 });
  });
});
