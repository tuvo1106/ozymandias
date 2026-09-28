import { describe, expect, it } from "vitest";
import { completionContext } from "./queryContext";

/** The context with the caret at the `|` in `marked`. */
function at(marked: string) {
  const caret = marked.indexOf("|");
  return completionContext(marked.replace("|", ""), caret);
}

describe("completionContext", () => {
  it.each([
    ["|", { kind: "expression", prefix: "", from: 0 }],
    ["su|", { kind: "expression", prefix: "su", from: 0 }],
    ["sum:x{*} + a|", { kind: "expression", prefix: "a", from: 11 }],
    ["abs(|", { kind: "expression", prefix: "" }],
    // After a comma inside a call the scanner is back at term position:
    // top()'s count is a number the author types, and an aggregator list
    // there is harmless where an empty one would hide a nested query.
    ["top(sum:x{*}, |", { kind: "expression", prefix: "" }],
  ])("term position: %s", (q, want) => {
    const got = at(q);
    expect(got).toMatchObject(want);
  });

  it("offers metrics after the aggregator's colon", () => {
    expect(at("sum:http.re|")).toMatchObject({ kind: "metric", prefix: "http.re", from: 4 });
    expect(at("SUM:|")).toMatchObject({ kind: "metric", prefix: "" });
  });

  it("replaces the whole word the caret is in, not only the part before it", () => {
    const ctx = at("sum:http.re|quest{*}");
    expect(ctx).toMatchObject({ kind: "metric", from: 4, to: 16 });
  });

  it("offers the metric's tag keys at the start of each matcher", () => {
    expect(at("sum:m{|")).toMatchObject({ kind: "tagKey", metric: "m", prefix: "" });
    expect(at("sum:m{a:b,|")).toMatchObject({ kind: "tagKey", metric: "m" });
    expect(at("sum:m{a:b, !ser|")).toMatchObject({ kind: "tagKey", metric: "m", prefix: "ser" });
  });

  it("offers the key's values after its colon", () => {
    expect(at("sum:m{service:ap|")).toMatchObject({
      kind: "tagValue",
      metric: "m",
      key: "service",
      prefix: "ap",
    });
    // A value is free text: colons and slashes inside it are part of it.
    expect(at("sum:m{url:http://h:80/p|")).toMatchObject({ kind: "tagValue", key: "url", prefix: "http://h:80/p" });
    // Keys are lower-cased as the lexer reads them, so the value lookup asks
    // for the key the store actually holds.
    expect(at("sum:m{Service:|")).toMatchObject({ kind: "tagValue", key: "service" });
  });

  it("offers values inside an IN list, one at a time", () => {
    expect(at("sum:m{status IN (500, 5|")).toMatchObject({ kind: "tagValue", key: "status", prefix: "5" });
    expect(at("sum:m{status in (|")).toMatchObject({ kind: "tagValue", key: "status", prefix: "" });
  });

  it("offers template variables after $", () => {
    expect(at("sum:m{$e|")).toMatchObject({ kind: "variable", prefix: "e" });
    expect(at("sum:m{a:b,$|")).toMatchObject({ kind: "variable", prefix: "" });
  });

  it("offers group keys inside by {…}", () => {
    expect(at("sum:m{*} by {|")).toMatchObject({ kind: "groupKey", metric: "m", prefix: "" });
    expect(at("sum:m{*} BY {route, st|")).toMatchObject({ kind: "groupKey", prefix: "st" });
    expect(at("sum:m{*} by{|")).toMatchObject({ kind: "groupKey" });
  });

  it("offers modifiers after a query's dot, and their first argument", () => {
    expect(at("sum:m{*}.|")).toMatchObject({ kind: "modifier", prefix: "" });
    expect(at("sum:m{*} by {r}.as_|")).toMatchObject({ kind: "modifier", prefix: "as_" });
    expect(at("sum:m{*}.rollup(|")).toMatchObject({ kind: "modifierArgument", modifier: "rollup", prefix: "" });
    expect(at("sum:m{*}.as_rate().fill(ze|")).toMatchObject({ kind: "modifierArgument", modifier: "fill", prefix: "ze" });
  });

  it("follows the metric of the query the caret is in, not the first one", () => {
    expect(at("sum:a{*} / sum:b{|")).toMatchObject({ kind: "tagKey", metric: "b" });
    expect(at("timeshift(sum:a{*}, -3600) + avg:c{k:|")).toMatchObject({ kind: "tagValue", metric: "c", key: "k" });
  });

  it("offers nothing where nothing can be completed", () => {
    for (const q of [
      "sum:m{*}.rollup(avg, 6|", // a number of seconds
      "top(sum:m{*}, 5, \"me|", // inside a string
      "sum:m{a:b}}|", // an extra brace the server will name
      "sum:1|", // a metric cannot start with a digit
      "12|",
    ]) {
      expect(at(q), q).toEqual({ kind: "none" });
    }
  });

  it("clamps a caret outside the text", () => {
    expect(completionContext("su", 99)).toMatchObject({ kind: "expression", prefix: "su" });
    expect(completionContext("su", -1)).toMatchObject({ kind: "expression", prefix: "" });
  });

  it("follows a query across lines", () => {
    expect(at("sum:m{\n  service:a|")).toMatchObject({ kind: "tagValue", key: "service", prefix: "a" });
  });
});
