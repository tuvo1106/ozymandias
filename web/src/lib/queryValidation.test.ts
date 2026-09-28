import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./metricsApi";
import {
  byteColumnToIndex,
  fetchValidation,
  lineAndColumn,
  readValidation,
  validationState,
} from "./queryValidation";

function answering(status: number, body: unknown) {
  return vi.fn(async () => new Response(JSON.stringify(body), { status }));
}

describe("readValidation", () => {
  it("reads the two documented shapes", () => {
    expect(readValidation({ ok: true, query: "sum:x{*}" })).toEqual({ kind: "ok", canonical: "sum:x{*}" });
    expect(readValidation({ ok: false, error: { msg: "expected '}'", col: 14 } })).toEqual({
      kind: "invalid",
      msg: "expected '}'",
      col: 14,
    });
  });

  it("calls anything else unrecognised rather than guessing", () => {
    for (const body of [
      null,
      [],
      "ok",
      { ok: true }, // a tick with no canonical text is not the documented answer
      { ok: false }, // refused, but nobody said where
      { ok: false, error: { msg: "x" } },
      { ok: false, error: { msg: "x", col: "14" } },
      { ok: false, error: { msg: "x", col: 1.5 } },
      { ok: "yes", query: "q" },
    ]) {
      expect(readValidation(body), JSON.stringify(body)).toEqual({ kind: "unrecognised" });
    }
  });
});

describe("fetchValidation", () => {
  it("posts the text and reads the answer", async () => {
    const fetchImpl = answering(200, { ok: false, error: { msg: "m", col: 3 } });
    await expect(fetchValidation("su", fetchImpl)).resolves.toEqual({ kind: "invalid", msg: "m", col: 3 });
    const [url, init] = fetchImpl.mock.calls[0] as unknown as [string, RequestInit];
    expect(url).toBe("/api/v1/query/validate");
    expect(JSON.parse(init.body as string)).toEqual({ q: "su" });
  });

  it("rejects when the request failed, so that is never mistaken for a verdict", async () => {
    await expect(fetchValidation("q", answering(500, { error: "boom" }))).rejects.toBeInstanceOf(ApiError);
  });
});

describe("byteColumnToIndex", () => {
  it("is the identity on ASCII, one-based to zero-based", () => {
    expect(byteColumnToIndex("sum:x{a:b by {k}", 14)).toBe(13);
    expect(byteColumnToIndex("abc", 1)).toBe(0);
  });

  it("allows one past the end, where 'found the end of the query' points", () => {
    expect(byteColumnToIndex("abc", 4)).toBe(3);
    expect(byteColumnToIndex("", 1)).toBe(0);
  });

  it("counts a multi-byte character as the bytes it is", () => {
    // é is bytes 12–13, so the brace after it is byte column 14 and string
    // index 12; counting characters would put column 14 on the second brace.
    const q = "sum:m{a:café}x";
    expect(byteColumnToIndex(q, 14)).toBe(12);
    expect(byteColumnToIndex(q, 15)).toBe(13);
    expect(q[13]).toBe("x");
    // An astral character is four bytes and two UTF-16 units.
    const astral = "sum:m{a:😀}x";
    expect(astral[byteColumnToIndex(astral, 14) as number]).toBe("x");
  });

  it("declines a column that is not at a character", () => {
    expect(byteColumnToIndex("café", 5)).toBeUndefined(); // inside the é
    expect(byteColumnToIndex("abc", 5)).toBeUndefined();
    expect(byteColumnToIndex("abc", 0)).toBeUndefined();
    expect(byteColumnToIndex("abc", 1.5)).toBeUndefined();
  });
});

describe("lineAndColumn", () => {
  it("counts lines and characters the way a reader does", () => {
    expect(lineAndColumn("sum:m{\n  a:b", 9)).toEqual({ line: 2, column: 3 });
    expect(lineAndColumn("abc", 0)).toEqual({ line: 1, column: 1 });
    expect(lineAndColumn("é😀x", 3)).toEqual({ line: 1, column: 3 });
  });
});

describe("validationState", () => {
  const ok = { kind: "ok" as const, canonical: "sum:x{*}" };

  it("does not check a blank query", () => {
    expect(validationState("  ", { asked: "  ", answer: ok })).toEqual({ kind: "blank" });
  });

  it("is checking until this exact text has an answer", () => {
    expect(validationState("sum:x{*}", undefined)).toEqual({ kind: "checking" });
    expect(validationState("sum:x{*}", { asked: "sum:x{*}" })).toEqual({ kind: "checking" });
  });

  it("never draws an answer onto text it was not about", () => {
    const stale = { asked: "sum:x{a:b by {k}", answer: { kind: "invalid" as const, msg: "m", col: 14 } };
    expect(validationState("sum:x{a:b} by {k}", stale)).toEqual({ kind: "checking" });
    // Not even a trailing space: the column is computed against the text.
    expect(validationState("sum:x{*} ", { asked: "sum:x{*}", answer: ok })).toEqual({ kind: "checking" });
  });

  it("underlines where the column lands, and says when it cannot", () => {
    expect(validationState("abc", { asked: "abc", answer: { kind: "invalid", msg: "m", col: 2 } })).toEqual({
      kind: "invalid",
      msg: "m",
      col: 2,
      index: 1,
    });
    expect(validationState("abc", { asked: "abc", answer: { kind: "invalid", msg: "m", col: 40 } })).toMatchObject({
      kind: "invalid",
      index: undefined,
    });
  });

  it("keeps a failed request and an unreadable answer apart from verdicts", () => {
    expect(validationState("q", { asked: "q", error: new ApiError("ozyd is unreachable") })).toEqual({
      kind: "failed",
      message: "ozyd is unreachable",
    });
    expect(validationState("q", { asked: "q", answer: { kind: "unrecognised" } })).toEqual({ kind: "unrecognised" });
    expect(validationState("q", { asked: "q", answer: ok })).toEqual({ kind: "ok", canonical: "sum:x{*}" });
  });
});
