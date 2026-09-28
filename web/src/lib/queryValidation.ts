/**
 * Asking ozyd whether a query parses, and turning the answer into something
 * the editor can underline.
 *
 * `POST /api/v1/query/validate` answers 200 whether or not the query parses —
 * the request is well formed either way — with `{ok:true, query}` or
 * `{ok:false, error:{msg, col}}`. Three things make that less simple than it
 * looks, and each is a way to draw a wrong underline or a false tick:
 *
 *   - **`col` counts bytes, and a JavaScript string counts UTF-16 units.**
 *     `service:café` puts every column after the `é` one off, which is an
 *     underline under the wrong character: the error message is right and the
 *     picture contradicts it. [[byteColumnToIndex]] converts, and says so
 *     when the column lands somewhere no character is.
 *   - **An answer is for the text that was asked**, and the editor has moved
 *     on by the time it lands. An underline at column 14 drawn onto text the
 *     author has since changed points at a character that did nothing wrong.
 *     [[validationState]] checks the text an answer belongs to before it
 *     shows anything.
 *   - **"Could not ask" is not "invalid" and not "valid".** A failed request
 *     and an answer this build cannot read each get their own state: showing
 *     either as a tick tells the author a query is fine when nobody checked
 *     it, and showing either as an error blames the query for the network.
 */
import { ApiError, postJSON } from "./metricsApi";

type FetchLike = typeof fetch;

/** What ozyd said about one query. */
export type ValidationAnswer =
  /** It parses; `canonical` is the spelling a dashboard stores. */
  | { kind: "ok"; canonical: string }
  /** It does not; `col` is a 1-based **byte** column into the text. */
  | { kind: "invalid"; msg: string; col: number }
  /** ozyd answered with something this build does not understand. */
  | { kind: "unrecognised" };

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Reads a validate response. Total: anything that is not exactly one of the
 * two documented shapes is `unrecognised` rather than a guess, because both
 * guesses available are wrong in a way the author would act on.
 */
export function readValidation(body: unknown): ValidationAnswer {
  if (!isRecord(body)) return { kind: "unrecognised" };
  if (body.ok === true && typeof body.query === "string")
    return { kind: "ok", canonical: body.query };
  if (
    body.ok === false &&
    isRecord(body.error) &&
    typeof body.error.msg === "string" &&
    typeof body.error.col === "number" &&
    Number.isInteger(body.error.col)
  )
    return { kind: "invalid", msg: body.error.msg, col: body.error.col };
  return { kind: "unrecognised" };
}

/**
 * Asks ozyd whether `q` parses. Rejects only when the *request* failed; a
 * query that does not parse is an ordinary answer.
 */
export async function fetchValidation(
  q: string,
  fetchImpl: FetchLike = fetch,
  signal?: AbortSignal,
): Promise<ValidationAnswer> {
  return readValidation(
    await postJSON("/api/v1/query/validate", { q }, fetchImpl, signal),
  );
}

/** How many UTF-8 bytes a code point takes. */
function utf8Length(codePoint: number): number {
  if (codePoint < 0x80) return 1;
  if (codePoint < 0x800) return 2;
  if (codePoint < 0x10000) return 3;
  return 4;
}

/**
 * The string index of a 1-based byte column, or undefined when the column is
 * not at the start of a character in `text`.
 *
 * One past the end is a real answer — "expected '}' but found the end of the
 * query" points there — and is returned as `text.length`. Anything else that
 * does not land on a character boundary (inside a multi-byte character, before
 * the start, past the end) is undefined, because the only alternative is to
 * underline a nearby character and let the reader believe it is the culprit.
 */
export function byteColumnToIndex(
  text: string,
  col: number,
): number | undefined {
  if (!Number.isInteger(col) || col < 1) return undefined;
  const target = col - 1;
  let bytes = 0;
  let index = 0;
  for (const ch of text) {
    if (bytes === target) return index;
    if (bytes > target) return undefined;
    bytes += utf8Length(ch.codePointAt(0) as number);
    index += ch.length;
  }
  return bytes === target ? index : undefined;
}

/** A string index as the 1-based line and column a person counts in. */
export function lineAndColumn(
  text: string,
  index: number,
): { line: number; column: number } {
  const before = text.slice(0, index);
  const lines = before.split("\n");
  return {
    line: lines.length,
    column: [...(lines[lines.length - 1] ?? "")].length + 1,
  };
}

/**
 * Everything the editor can say about the query in front of it. Each member
 * renders differently, and none of them is the absence of the others.
 */
export type ValidationState =
  /** Nothing typed. Not checked: a blank query is skipped, not refused. */
  | { kind: "blank" }
  /** This text has not been answered yet — waiting to ask, or asking. */
  | { kind: "checking" }
  | { kind: "ok"; canonical: string }
  /**
   * It does not parse. `index` is where to underline, or undefined when the
   * column ozyd named is not a character of this text — the message still
   * shows, the underline does not.
   */
  | { kind: "invalid"; msg: string; col: number; index: number | undefined }
  /** ozyd answered, and this build cannot read what it said. */
  | { kind: "unrecognised" }
  /** The question could not be asked; nothing is known about the query. */
  | { kind: "failed"; message: string };

/** One answer and the text it answers. */
export interface AskedValidation {
  /** The exact text that was sent. */
  asked: string;
  answer?: ValidationAnswer;
  error?: Error | null;
}

/**
 * What to show for `text`, given the last thing ozyd said and which text it
 * said it about.
 *
 * The answer is used only when it is about *this* text. The comparison is
 * exact rather than trimmed: `sum:x{a:b by {k}` and the same with one more
 * character typed have their errors in different places, and an underline is
 * only as good as the text it was computed against.
 */
export function validationState(
  text: string,
  last: AskedValidation | undefined,
): ValidationState {
  if (text.trim() === "") return { kind: "blank" };
  if (!last || last.asked !== text) return { kind: "checking" };
  if (last.error)
    return {
      kind: "failed",
      message:
        last.error instanceof ApiError || last.error instanceof Error
          ? last.error.message
          : String(last.error),
    };
  const answer = last.answer;
  if (!answer) return { kind: "checking" };
  switch (answer.kind) {
    case "ok":
      return { kind: "ok", canonical: answer.canonical };
    case "invalid":
      return {
        kind: "invalid",
        msg: answer.msg,
        col: answer.col,
        index: byteColumnToIndex(text, answer.col),
      };
    case "unrecognised":
      return { kind: "unrecognised" };
  }
}
