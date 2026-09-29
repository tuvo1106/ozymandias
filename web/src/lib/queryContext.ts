/**
 * What is being typed at the caret of a metricql query: the autocomplete's
 * one question.
 *
 * **A scanner, not a parser**, and deliberately so. A parser answers "is this
 * a query", and the text under an editor's caret almost never is one — it is
 * `sum:http.req` or `avg:x{service:ap` — so a parser would fail on exactly the
 * input this exists for. What completion needs is weaker: which *position* in
 * the grammar the caret is in (an aggregator, a metric, a tag key, a value, a
 * modifier) and which metric and key the position belongs to. That is a state
 * machine over the text before the caret, which tolerates everything after it
 * being missing.
 *
 * It is tolerant in the other direction too: text that is simply wrong
 * (`sum:x{a:b}}`) lands in a state that offers nothing rather than throwing,
 * because the server's parse error is what tells the author it is wrong
 * (POST /api/v1/query/validate) and a second opinion from here would only be
 * able to disagree with it.
 *
 * The grammar this follows is docs/query-language.md; the words it completes
 * come from metricqlVocabulary.json, which a Go test generates from the
 * parser's own tables so the two cannot drift.
 */

/** The positions the editor can complete, and what each needs to know. */
export type CompletionContext =
  /** The start of a term: an aggregator (`sum:`) or a function (`abs(`). */
  | { kind: "expression"; from: number; to: number; prefix: string }
  /** After `agg:`, before `{`. */
  | { kind: "metric"; from: number; to: number; prefix: string }
  /** The start of a matcher inside `{…}`. */
  | {
      kind: "tagKey";
      from: number;
      to: number;
      prefix: string;
      metric: string;
    }
  /** After `key:` inside `{…}`, or inside `key IN (…)`. */
  | {
      kind: "tagValue";
      from: number;
      to: number;
      prefix: string;
      metric: string;
      key: string;
    }
  /** After `$` inside `{…}`: a template variable's name. */
  | { kind: "variable"; from: number; to: number; prefix: string }
  /** Inside `by {…}`. */
  | {
      kind: "groupKey";
      from: number;
      to: number;
      prefix: string;
      metric: string;
    }
  /** After the `.` that follows a query. */
  | { kind: "modifier"; from: number; to: number; prefix: string }
  /** The first argument of `.rollup(` or `.fill(`. */
  | {
      kind: "modifierArgument";
      from: number;
      to: number;
      prefix: string;
      modifier: string;
    }
  /** Nothing to offer here: a number, an operator, a string, a closed query. */
  | { kind: "none" };

type State =
  | "expr" // expecting a term
  | "word" // inside an identifier at term position
  | "metric" // after agg:
  | "key" // at a matcher start inside { }
  | "keyWord" // inside a tag key
  | "afterKey" // after a key, expecting ':' or IN
  | "value" // inside a value after key:
  | "inOpen" // after IN, expecting (
  | "inValue" // inside an IN list
  | "afterIn" // after the IN list's ), expecting , or }
  | "variable" // inside $name
  | "afterQuery" // after a query's } — by, a modifier, an operator
  | "byWord" // typing "by"
  | "byOpen" // after by, expecting {
  | "groupKey" // inside by { }
  | "modName" // after .
  | "modArg" // first argument of a modifier
  | "modRest" // later arguments of a modifier
  | "string" // inside "…"
  | "number" // inside a number
  | "dead"; // something the scanner does not follow; offer nothing

const isLetter = (c: string) => /[A-Za-z]/.test(c);
const isNameChar = (c: string) => /[A-Za-z0-9_.]/.test(c);
const isKeyChar = (c: string) => /[A-Za-z0-9_.\-/]/.test(c);
const isSpace = (c: string) => c === " " || c === "\t" || c === "\n" || c === "\r";

/**
 * The completion context at `caret`.
 *
 * `from`…`to` is the span a chosen completion replaces: from the start of the
 * word being typed to the end of it, *including* the part after the caret, so
 * completing in the middle of `http.re|quest` replaces the whole name rather
 * than leaving `quest` stranded after the insertion.
 */
export function completionContext(text: string, caret: number): CompletionContext {
  const end = Math.max(0, Math.min(caret, text.length));
  let state: State = "expr";
  let start = 0; // where the current word began
  let metric = "";
  let key = "";
  let modifier = "";
  // Parentheses that are function calls or grouping, so a ',' or ')' at depth
  // returns to term position rather than being read as the end of a query.
  let depth = 0;

  const afterTerm = (c: string): State => {
    // After a complete term: an operator starts a new one, ')' closes a group,
    // ',' separates function arguments. Anything else is not followed.
    if ("+-*/".includes(c)) return "expr";
    if (c === ",") return depth > 0 ? "expr" : "dead";
    if (c === ")") {
      if (depth === 0) return "dead";
      depth--;
      return "afterQuery";
    }
    return isSpace(c) ? "afterQuery" : "dead";
  };

  for (let i = 0; i < end; i++) {
    const c = text[i] as string;
    switch (state) {
      case "expr":
        if (isSpace(c) || "+-*/".includes(c)) break;
        if (c === "(") {
          depth++;
          break;
        }
        if (c === '"') state = "string";
        else if (/[0-9]/.test(c)) state = "number";
        else if (isLetter(c)) {
          state = "word";
          start = i;
        } else state = "dead";
        break;
      case "word":
        if (isNameChar(c)) break;
        if (c === ":") {
          state = "metric";
          start = i + 1;
        } else if (c === "(") {
          depth++;
          state = "expr";
        } else state = "dead";
        break;
      case "metric":
        if (isNameChar(c) && !(i === start && !isLetter(c))) break;
        if (c === "{") {
          metric = text.slice(start, i).trim();
          state = "key";
        } else if (isSpace(c) && i === start) {
          start = i + 1;
        } else state = "dead";
        break;
      case "key":
        if (isSpace(c) || c === "!" || c === "," || c === "*") break;
        if (c === "}") state = "afterQuery";
        else if (c === "$") {
          state = "variable";
          start = i + 1;
        } else if (isLetter(c)) {
          state = "keyWord";
          start = i;
        } else state = "dead";
        break;
      case "keyWord":
        if (isKeyChar(c)) break;
        key = text.slice(start, i).toLowerCase();
        if (c === ":") {
          state = "value";
          start = i + 1;
        } else if (isSpace(c)) state = "afterKey";
        else state = "dead";
        break;
      case "afterKey":
        if (isSpace(c)) break;
        if (c === ":") {
          state = "value";
          start = i + 1;
        } else if (
          (c === "I" || c === "i") &&
          /^in\s/i.test(text.slice(i, i + 3))
        ) {
          state = "inOpen";
          i += 1; // the N; the space is skipped by inOpen
        } else state = "dead";
        break;
      case "value":
        if (c === ",") state = "key";
        else if (c === "}") state = "afterQuery";
        else if (c === "{") state = "dead";
        else if (isSpace(c) && i === start) start = i + 1;
        break;
      case "inOpen":
        if (isSpace(c)) break;
        if (c === "(") {
          state = "inValue";
          start = i + 1;
        } else state = "dead";
        break;
      case "inValue":
        if (c === ",") start = i + 1;
        else if (c === ")") state = "afterIn";
        else if (c === "{" || c === "}") state = "dead";
        else if (isSpace(c) && i === start) start = i + 1;
        break;
      case "afterIn":
        if (isSpace(c)) break;
        if (c === ",") state = "key";
        else if (c === "}") state = "afterQuery";
        else state = "dead";
        break;
      case "variable":
        if (isNameChar(c)) break;
        if (c === ",") state = "key";
        else if (c === "}") state = "afterQuery";
        else if (isSpace(c)) state = "afterIn"; // the same "expect , or }"
        else state = "dead";
        break;
      case "afterQuery":
        if (c === ".") {
          state = "modName";
          start = i + 1;
        } else if (c === "b" || c === "B") {
          state = "byWord";
          start = i;
        } else state = afterTerm(c);
        break;
      case "byWord":
        if (isLetter(c)) break;
        if (text.slice(start, i).toLowerCase() !== "by") state = "dead";
        else if (c === "{") state = "groupKey";
        else if (isSpace(c)) state = "byOpen";
        else state = "dead";
        start = i + 1;
        break;
      case "byOpen":
        if (isSpace(c)) break;
        if (c === "{") {
          state = "groupKey";
          start = i + 1;
        } else state = "dead";
        break;
      case "groupKey":
        if (isKeyChar(c)) break;
        if (c === "," || isSpace(c)) start = i + 1;
        else if (c === "}") state = "afterQuery";
        else state = "dead";
        break;
      case "modName":
        if (isNameChar(c)) break;
        if (c === "(") {
          modifier = text.slice(start, i).toLowerCase();
          state = "modArg";
          start = i + 1;
        } else state = "dead";
        break;
      case "modArg":
        if (isNameChar(c)) break;
        if (isSpace(c) && i === start) start = i + 1;
        else if (c === ",") state = "modRest";
        else if (c === ")") state = "afterQuery";
        else if (!isSpace(c)) state = "dead";
        break;
      case "modRest":
        if (c === ")") state = "afterQuery";
        break;
      case "string":
        if (c === "\\") i++;
        else if (c === '"') state = "afterQuery";
        break;
      case "number":
        if (/[0-9.eE]/.test(c)) break;
        state = afterTerm(c);
        break;
      case "dead":
        return { kind: "none" };
    }
  }

  // The rest of the word the caret is inside, so a completion replaces it.
  const rest = (pred: (c: string) => boolean) => {
    let to = end;
    while (to < text.length && pred(text[to] as string)) to++;
    return to;
  };
  const prefix = text.slice(start, end);
  switch (state) {
    case "expr":
      return { kind: "expression", from: end, to: rest(isNameChar), prefix: "" };
    case "word":
      return { kind: "expression", from: start, to: rest(isNameChar), prefix };
    case "metric":
      return { kind: "metric", from: start, to: rest(isNameChar), prefix };
    case "key":
      return { kind: "tagKey", from: end, to: rest(isKeyChar), prefix: "", metric };
    case "keyWord":
      return { kind: "tagKey", from: start, to: rest(isKeyChar), prefix, metric };
    case "value":
    case "inValue": {
      const stop = state === "value" ? ",{}" : ",{})";
      return {
        kind: "tagValue",
        from: start,
        to: rest((c) => !stop.includes(c)),
        prefix,
        metric,
        key,
      };
    }
    case "variable":
      return { kind: "variable", from: start, to: rest(isNameChar), prefix };
    case "groupKey":
      return { kind: "groupKey", from: start, to: rest(isKeyChar), prefix, metric };
    case "modName":
      return { kind: "modifier", from: start, to: rest(isNameChar), prefix };
    case "modArg":
      return {
        kind: "modifierArgument",
        from: start,
        to: rest(isNameChar),
        prefix,
        modifier,
      };
    default:
      return { kind: "none" };
  }
}
