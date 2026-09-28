/**
 * What the query editor offers at the caret, and what choosing it does.
 *
 * [[completionContext]] says *where* the caret is; this says *what goes
 * there*. Some positions are answered from the grammar's own vocabulary
 * (aggregators, functions, modifiers — metricqlVocabulary.json, generated
 * from the Go parser), some from the dashboard (template variables), and
 * some from the store (metrics, tag keys, tag values).
 *
 * The last kind is where a list goes wrong quietly, so its states are a
 * union rather than an array: "no suggestions" is a claim about the store,
 * and it is only made when the store has actually answered, for this exact
 * question, with nothing. Still asking, could not ask, and "cannot ask until
 * the query names a metric" are each said as themselves.
 */
import vocabulary from "./metricqlVocabulary.json";
import type { CompletionContext } from "./queryContext";

/** One thing the editor can insert. */
export interface Completion {
  /** What the list shows. */
  label: string;
  /** What replaces the word at the caret. */
  insert: string;
  /** A hint beside the label: "aggregator", "function", … */
  detail?: string;
}

/** The list at the caret. */
export type CompletionList =
  /** Nothing is completed here. */
  | { status: "none" }
  /** A lookup needs something the text does not say yet. */
  | { status: "blocked"; message: string }
  | { status: "loading" }
  | { status: "failed"; message: string }
  /** Answered, for this exact question, with nothing matching. */
  | { status: "empty"; message: string }
  | { status: "ready"; items: Completion[] };

/** The vocabulary, typed. */
export const VOCABULARY = vocabulary as {
  aggregators: string[];
  functions: string[];
  modifiers: { name: string; argument: string }[];
  rollup_methods: string[];
  fill_modes: string[];
};

const matches = (prefix: string) => {
  const p = prefix.toLowerCase();
  return (s: string) => s.toLowerCase().startsWith(p);
};

function ready(items: Completion[], nothing: string): CompletionList {
  return items.length ? { status: "ready", items } : { status: "empty", message: nothing };
}

/**
 * The list for a position answered without the store: the grammar's words,
 * or the dashboard's variables. Undefined for a position the store answers.
 */
export function localCompletions(ctx: CompletionContext, variables: readonly string[]): CompletionList | undefined {
  switch (ctx.kind) {
    case "none":
      return { status: "none" };
    case "expression": {
      const m = matches(ctx.prefix);
      return ready(
        [
          ...VOCABULARY.aggregators.filter(m).map((a) => ({ label: a, insert: `${a}:`, detail: "aggregator" })),
          ...VOCABULARY.functions.filter(m).map((f) => ({ label: f, insert: `${f}(`, detail: "function" })),
        ],
        `No aggregator or function starts with "${ctx.prefix}".`,
      );
    }
    case "modifier":
      return ready(
        VOCABULARY.modifiers
          .filter((mod) => matches(ctx.prefix)(mod.name))
          .map((mod) => ({
            label: mod.name,
            insert: mod.argument ? `${mod.name}(` : `${mod.name}()`,
            detail: "modifier",
          })),
        `No modifier starts with "${ctx.prefix}".`,
      );
    case "modifierArgument": {
      const mod = VOCABULARY.modifiers.find((x) => x.name === ctx.modifier);
      const list =
        mod?.argument === "rollup_methods"
          ? VOCABULARY.rollup_methods
          : mod?.argument === "fill_modes"
            ? VOCABULARY.fill_modes
            : undefined;
      if (!list) {
        // An unknown modifier, or one that takes no argument: the parse error
        // will name it; an empty list here would read as "no methods exist".
        return { status: "none" };
      }
      return ready(
        list.filter(matches(ctx.prefix)).map((v) => ({ label: v, insert: v, detail: mod?.argument === "fill_modes" ? "fill mode" : "rollup method" })),
        `No ${ctx.modifier} argument starts with "${ctx.prefix}".`,
      );
    }
    case "variable":
      if (variables.length === 0)
        return { status: "empty", message: "This dashboard declares no template variables; add one in its settings." };
      return ready(
        variables.filter(matches(ctx.prefix)).map((v) => ({ label: `$${v}`, insert: v, detail: "variable" })),
        `No variable starts with "${ctx.prefix}".`,
      );
    default:
      return undefined;
  }
}

/** One store lookup's state, as the hook hands it over. */
export interface Lookup {
  /** The names the store answered with, if it has answered *this* question. */
  data?: readonly string[];
  loading: boolean;
  error?: Error | null;
}

/**
 * The list for a position the store answers.
 *
 * `lookup.data` must be the answer to the question this context asks — the
 * hook withholds a placeholder answer from a previous prefix, because
 * filtering the answer for `x` by the prefix `ht` would announce that no
 * metric starts with `ht`.
 */
export function storeCompletions(ctx: CompletionContext, lookup: Lookup): CompletionList {
  if (ctx.kind === "tagKey" || ctx.kind === "groupKey" || ctx.kind === "tagValue") {
    if (ctx.metric === "")
      return { status: "blocked", message: "Tag suggestions need the metric first." };
  }
  if (lookup.error) return { status: "failed", message: `Suggestions are unavailable: ${lookup.error.message}` };
  if (!lookup.data) return lookup.loading ? { status: "loading" } : { status: "none" };
  const m = matches(ctx.kind === "none" ? "" : ctx.prefix);
  const names = lookup.data.filter(m);
  switch (ctx.kind) {
    case "metric":
      return ready(
        names.map((n) => ({ label: n, insert: `${n}{`, detail: "metric" })),
        ctx.prefix ? `No metric starts with "${ctx.prefix}".` : "The store holds no metrics yet.",
      );
    case "tagKey":
      return ready(
        names.map((n) => ({ label: n, insert: `${n}:`, detail: "tag key" })),
        `No tag key on ${ctx.metric} starts with "${ctx.prefix}".`,
      );
    case "groupKey":
      return ready(
        names.map((n) => ({ label: n, insert: n, detail: "tag key" })),
        `No tag key on ${ctx.metric} starts with "${ctx.prefix}".`,
      );
    case "tagValue":
      return ready(
        names.map((n) => ({ label: n, insert: n, detail: `${ctx.key} value` })),
        // The values endpoint returns a capped list, so "none match" is only
        // true of the values it returned — said, rather than implied complete.
        `No value of ${ctx.key} on ${ctx.metric} starts with "${ctx.prefix}" among those the store returned.`,
      );
    default:
      return { status: "none" };
  }
}

/**
 * The text after choosing a completion, and where the caret goes.
 *
 * The completion's closing punctuation is not doubled: choosing a metric in
 * `sum:ht|{*}` inserts `http.request.count`, not `http.request.count{{*}`,
 * because the brace is already there.
 */
export function applyCompletion(
  text: string,
  ctx: Exclude<CompletionContext, { kind: "none" }>,
  item: Completion,
): { text: string; caret: number } {
  let insert = item.insert;
  const next = text[ctx.to];
  const last = insert[insert.length - 1];
  if (last !== undefined && ":{(".includes(last) && next === last) insert = insert.slice(0, -1);
  if (insert.endsWith("()") && next === "(") insert = insert.slice(0, -2);
  const out = text.slice(0, ctx.from) + insert + text.slice(ctx.to);
  const caret = ctx.from + insert.length + (insert === item.insert ? 0 : 1);
  return { text: out, caret };
}
