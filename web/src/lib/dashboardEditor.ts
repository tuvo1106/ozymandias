/**
 * The dashboard editor's model: a draft definition, the edits that change it,
 * and the rules for what each widget type uses.
 *
 * **The draft is the definition, not a form over it.** Every edit is a pure
 * function from one definition to the next, and anything the editor does not
 * touch survives untouched — including fields and values this build has never
 * heard of. A stored definition is served back without being re-validated
 * (api.md), so a newer `ozyd`, a hand-edited row or an import can hand the
 * editor a widget type, reducer, colour or key this bundle does not know. The
 * two easy mistakes with such a value are both silent: dropping it (the save
 * quietly deletes something) and rendering it through a control that cannot
 * show it (a `<select>` whose value matches no option *displays its first
 * option*, so the editor shows "last" for a reducer that says "p42"). This
 * file's job is to make those cases values the UI must render: see
 * [[readEnum]], [[unusedFields]] and [[unknownKeys]].
 *
 * The rules mirror `internal/dashboard/validate.go`. They are applied as
 * *information* — what the editor offers, and what it flags — never as
 * silent correction; the server remains the one that refuses, and its message
 * (which names every problem) is shown as it is.
 */
import {
  COLUMNS,
  type ConditionalFormat,
  type Dashboard,
  type DashboardQuery,
  type Layout,
  type Widget,
  type WidgetType,
} from "./dashboard";
import { clampLayout, newWidgetLayout, pushDown } from "./gridLayout";

/** What a widget type uses. One entry per type, which the compiler requires. */
export interface TypeRules {
  /** A person's name for the type. */
  label: string;
  /** How many queries: none (a note), exactly one (a heatmap), or 1…10. */
  queries: "none" | "one" | "many";
  /** Whether each query needs a reducer, or must not have one. */
  reducer: "required" | "refused";
  /** Per-query `display` (line, area, …). */
  display: boolean;
  yaxis: boolean;
  precision: boolean;
  conditionalFormats: boolean;
  limit: boolean;
  markdown: boolean;
  /** The aggregator the query must, or must not, use. */
  dist: "required" | "refused" | "n/a";
  /** The size a new widget of this type starts at. */
  size: { w: number; h: number };
}

/**
 * Every type's rules. `satisfies Record<WidgetType, …>` is what makes adding
 * a seventh type fail to compile here until someone decides what it uses —
 * the pattern REDUCERS and RENDERERS use, for the same reason.
 */
export const TYPE_RULES = {
  timeseries: {
    label: "Timeseries",
    queries: "many",
    reducer: "refused",
    display: true,
    yaxis: true,
    precision: false,
    conditionalFormats: false,
    limit: false,
    markdown: false,
    dist: "refused",
    size: { w: 6, h: 3 },
  },
  query_value: {
    label: "Query value",
    queries: "many",
    reducer: "required",
    display: false,
    yaxis: false,
    precision: true,
    conditionalFormats: true,
    limit: false,
    markdown: false,
    dist: "refused",
    size: { w: 3, h: 2 },
  },
  toplist: {
    label: "Toplist",
    queries: "many",
    reducer: "required",
    display: false,
    yaxis: false,
    precision: false,
    conditionalFormats: false,
    limit: true,
    markdown: false,
    dist: "refused",
    size: { w: 4, h: 3 },
  },
  table: {
    label: "Table",
    queries: "many",
    reducer: "required",
    display: false,
    yaxis: false,
    precision: true,
    conditionalFormats: true,
    limit: false,
    markdown: false,
    dist: "refused",
    size: { w: 6, h: 3 },
  },
  heatmap: {
    label: "Heatmap",
    queries: "one",
    reducer: "refused",
    display: false,
    yaxis: true,
    precision: false,
    conditionalFormats: false,
    limit: false,
    markdown: false,
    dist: "required",
    size: { w: 6, h: 3 },
  },
  note: {
    label: "Note",
    queries: "none",
    reducer: "refused",
    display: false,
    yaxis: false,
    precision: false,
    conditionalFormats: false,
    limit: false,
    markdown: true,
    dist: "n/a",
    size: { w: 4, h: 2 },
  },
} as const satisfies Record<WidgetType, TypeRules>;

/** The widget types this build knows, in the order the editor offers them. */
export const WIDGET_TYPES = Object.keys(TYPE_RULES) as WidgetType[];

/** Whether this build knows a widget type. */
export function isWidgetType(v: unknown): v is WidgetType {
  return typeof v === "string" && Object.hasOwn(TYPE_RULES, v);
}

/** The rules for a type, or undefined for one this build does not know. */
export function rulesFor(type: string): TypeRules | undefined {
  return isWidgetType(type) ? TYPE_RULES[type] : undefined;
}

/**
 * A field's value as one of the three things it can be.
 *
 *   - `absent` — not in the definition. Distinct from any value, because for
 *     most of these absence means something ("line", "linear", "no reducer
 *     chosen yet").
 *   - `known` — one of the values this build offers.
 *   - `unknown` — present and not one this build knows. `raw` is its text,
 *     so the control can show exactly what the definition says.
 */
export type EnumReading<T extends string> =
  | { kind: "absent" }
  | { kind: "known"; value: T }
  | { kind: "unknown"; raw: string };

/**
 * Reads an enum-valued field. Total over whatever arrived: a number, an
 * object or an empty string where a string was expected is `unknown` rather
 * than absent, because it *is* in the definition and saving will send it.
 */
export function readEnum<T extends string>(
  value: unknown,
  known: readonly T[],
): EnumReading<T> {
  if (value === undefined) return { kind: "absent" };
  if (typeof value === "string" && (known as readonly string[]).includes(value))
    return { kind: "known", value: value as T };
  return { kind: "unknown", raw: typeof value === "string" ? value : JSON.stringify(value) };
}

/** How a timeseries query may be drawn; absent means `line`. */
export const DISPLAYS = ["line", "area", "bars", "points"] as const;
/** How a line reduces to one number, as `isReducer` accepts. */
export const REDUCER_OPTIONS = ["last", "avg", "sum", "min", "max"] as const;
/** A conditional format's comparisons. */
export const OPS = [">", ">=", "<", "<=", "=", "!="] as const;
/** A y-axis scale; absent means `linear`. */
export const SCALES = ["linear", "log"] as const;
/**
 * The palette names a conditional format may use. `gray` and `grey` are both
 * drawn; the editor offers one spelling and accepts either.
 */
export const FORMAT_COLORS = ["red", "yellow", "green", "blue", "grey", "gray"] as const;
/** One of [[FORMAT_COLORS]]. */
export type FormatColor = (typeof FORMAT_COLORS)[number];

/**
 * A number typed into a field whose absence means something, read without
 * losing that distinction: `""` is absent, `"0"` is zero, and text that is
 * not a number is neither — it is kept in the input and not written, so a
 * half-typed `-` does not become a `NaN` in the definition (which
 * `JSON.stringify` would then save as `null`).
 */
export type NumberReading =
  | { kind: "absent" }
  | { kind: "number"; value: number }
  | { kind: "invalid"; reason: string };

/** Reads an optional numeric input. */
export function readNumber(
  text: string,
  opts: { integer?: boolean; min?: number; max?: number } = {},
): NumberReading {
  const t = text.trim();
  if (t === "") return { kind: "absent" };
  const v = Number(t);
  if (!Number.isFinite(v)) return { kind: "invalid", reason: "a number" };
  if (opts.integer && !Number.isInteger(v))
    return { kind: "invalid", reason: "a whole number" };
  if (opts.min !== undefined && v < opts.min)
    return { kind: "invalid", reason: `at least ${opts.min}` };
  if (opts.max !== undefined && v > opts.max)
    return { kind: "invalid", reason: `at most ${opts.max}` };
  return { kind: "number", value: v };
}

/** A field a widget carries that its type does not use. */
export interface UnusedField {
  /** Where it is: a widget field, or one query's field. */
  field: "queries" | "markdown" | "limit" | "precision" | "yaxis" | "conditional_formats" | "display" | "reducer";
  /** The query it belongs to, for a per-query field. */
  query?: number;
  /** Why the server will refuse it. */
  message: string;
}

/**
 * Every field a widget carries that its type does not use.
 *
 * These are what a type change leaves behind, and what an import can bring
 * in. The server refuses each of them ("a field that belongs to another type
 * is an error rather than ignored", dashboards.md), so the editor lists them
 * with a way to remove each — rather than dropping them itself on a type
 * change, which would throw away a conditional format the author flipped to
 * a timeseries and back to look at.
 *
 * Presence is by key, not truthiness: `precision: 0` is present, `limit: 0`
 * is the server's "not set" and is not.
 */
export function unusedFields(w: Widget): UnusedField[] {
  const rules = rulesFor(w.type);
  if (!rules) return []; // an unknown type's rules are unknown too
  const name = rules.label.toLowerCase();
  const out: UnusedField[] = [];
  const has = (k: keyof Widget) => Object.hasOwn(w, k) && w[k] !== undefined;
  if (rules.queries === "none" && (w.queries?.length ?? 0) > 0)
    out.push({ field: "queries", message: `A ${name} has no queries.` });
  if (!rules.markdown && has("markdown") && w.markdown !== "")
    out.push({ field: "markdown", message: `Markdown belongs to a note, not a ${name}.` });
  if (!rules.limit && has("limit") && w.limit !== 0)
    out.push({ field: "limit", message: `A limit belongs to a toplist, not a ${name}.` });
  if (!rules.precision && has("precision"))
    out.push({ field: "precision", message: `Precision belongs to a query value or a table, not a ${name}.` });
  if (!rules.yaxis && has("yaxis"))
    out.push({ field: "yaxis", message: `A y-axis belongs to a timeseries or a heatmap, not a ${name}.` });
  if (!rules.conditionalFormats && (w.conditional_formats?.length ?? 0) > 0)
    out.push({
      field: "conditional_formats",
      message: `Conditional formats belong to a query value or a table, not a ${name}.`,
    });
  if (rules.queries !== "none") {
    (w.queries ?? []).forEach((q, i) => {
      if (!rules.display && q.display !== undefined)
        out.push({ field: "display", query: i, message: `Display belongs to a timeseries, not a ${name}.` });
      if (rules.reducer === "refused" && q.reducer !== undefined)
        out.push({
          field: "reducer",
          query: i,
          message: `A ${name} draws every bucket, so a reducer would be ignored.`,
        });
    });
  }
  return out;
}

/** The fields this build reads on each object of a definition. */
const KNOWN_KEYS = {
  dashboard: ["uid", "title", "description", "template", "template_vars", "widgets"],
  widget: [
    "id",
    "type",
    "title",
    "layout",
    "queries",
    "yaxis",
    "precision",
    "conditional_formats",
    "limit",
    "markdown",
  ],
  query: ["q", "name", "display", "reducer"],
} as const;

/**
 * Keys on an object this build does not read. The editor keeps them — an
 * edit spreads the object it changes, so they survive every save — and lists
 * them, because a key the page cannot show is otherwise a key nobody knows is
 * there.
 */
export function unknownKeys(obj: object, kind: keyof typeof KNOWN_KEYS): string[] {
  const known: readonly string[] = KNOWN_KEYS[kind];
  return Object.keys(obj).filter((k) => !known.includes(k));
}

/**
 * The aggregator a query starts with, read off the text. A heuristic, used
 * only to *hint* at the heatmap's `dist:` rule while typing; the server's
 * refusal on save is the authority. Undefined when the text does not start
 * `word:` — an arithmetic expression, a function call, a blank.
 */
export function leadingAggregator(q: string): string | undefined {
  return /^\s*([A-Za-z][A-Za-z0-9]*)\s*:/.exec(q)?.[1]?.toLowerCase();
}

/** An object with `patch` applied, and every key the patch sets to undefined removed. */
function withFields<T extends object>(obj: T, patch: Partial<T>): T {
  const out = { ...obj } as Record<string, unknown>;
  for (const [k, v] of Object.entries(patch)) {
    if (v === undefined) delete out[k];
    else out[k] = v;
  }
  return out as T;
}

/** An id no widget on the dashboard has: `w1`, `w2`, … */
export function nextWidgetId(widgets: readonly Widget[]): string {
  const taken = new Set(widgets.map((w) => w.id));
  for (let n = widgets.length + 1; ; n++) if (!taken.has(`w${n}`)) return `w${n}`;
}

/** Every edit the editor can make. */
export type EditorAction =
  | { type: "replace"; dashboard: Dashboard }
  | { type: "setMeta"; patch: Partial<Pick<Dashboard, "title" | "description" | "uid" | "template_vars">> }
  | { type: "addWidget"; widgetType: WidgetType }
  | { type: "duplicateWidget"; id: string }
  | { type: "removeWidget"; id: string }
  | { type: "setWidget"; id: string; patch: Partial<Widget> }
  | { type: "setType"; id: string; to: WidgetType }
  | { type: "setLayout"; id: string; layout: Layout; settle: boolean }
  | { type: "removeUnused"; id: string; field: UnusedField }
  | { type: "addQuery"; id: string }
  | { type: "removeQuery"; id: string; index: number }
  | { type: "setQuery"; id: string; index: number; patch: Partial<DashboardQuery> }
  | { type: "addFormat"; id: string }
  | { type: "removeFormat"; id: string; index: number }
  | { type: "setFormat"; id: string; index: number; patch: Partial<ConditionalFormat> };

/** A new widget of a type, with nothing chosen on its behalf. */
function newWidget(type: WidgetType, widgets: readonly Widget[]): Widget {
  const rules = TYPE_RULES[type];
  const base: Widget = {
    id: nextWidgetId(widgets),
    type,
    title: `New ${rules.label.toLowerCase()}`,
    layout: newWidgetLayout(widgets, rules.size.w, rules.size.h),
  };
  // A note starts with empty markdown and a query widget with one empty
  // query: both are what the author fills in next, and both are refused on
  // save if left empty — which is the truth about them. No reducer is chosen
  // for a toplist: which number a toplist ranks by is the question the
  // widget exists to answer, and a default here is the silent choice
  // dashboards.md warns about.
  return rules.queries === "none" ? { ...base, markdown: "" } : { ...base, queries: [{ q: "" }] };
}

function mapWidget(d: Dashboard, id: string, f: (w: Widget) => Widget): Dashboard {
  return { ...d, widgets: d.widgets.map((w) => (w.id === id ? f(w) : w)) };
}

/**
 * Applies one edit. Pure; an edit naming a widget or query that does not
 * exist changes nothing rather than throwing, because the UI that sent it can
 * be one render behind a removal.
 */
export function editDashboard(d: Dashboard, a: EditorAction): Dashboard {
  switch (a.type) {
    case "replace":
      return a.dashboard;
    case "setMeta":
      return withFields(d, a.patch as Partial<Dashboard>);
    case "addWidget":
      return { ...d, widgets: [...d.widgets, newWidget(a.widgetType, d.widgets)] };
    case "duplicateWidget": {
      const src = d.widgets.find((w) => w.id === a.id);
      if (!src) return d;
      const copy: Widget = {
        ...structuredClone(src),
        id: nextWidgetId(d.widgets),
        layout: newWidgetLayout(d.widgets, src.layout.w, src.layout.h),
      };
      return { ...d, widgets: [...d.widgets, copy] };
    }
    case "removeWidget":
      return { ...d, widgets: d.widgets.filter((w) => w.id !== a.id) };
    case "setWidget":
      return mapWidget(d, a.id, (w) => withFields(w, a.patch));
    case "setType":
      // Only the type changes. What the new type does not use stays, and is
      // listed by [[unusedFields]] for the author to remove — see there.
      return mapWidget(d, a.id, (w) => {
        const next: Widget = { ...w, type: a.to };
        const rules = TYPE_RULES[a.to];
        if (rules.queries !== "none" && (next.queries?.length ?? 0) === 0) next.queries = [{ q: "" }];
        if (rules.markdown && next.markdown === undefined) next.markdown = "";
        return next;
      });
    case "setLayout": {
      const moved = mapWidget(d, a.id, (w) => ({ ...w, layout: clampLayout(a.layout) }));
      return a.settle ? { ...moved, widgets: pushDown(moved.widgets, a.id) } : moved;
    }
    case "removeUnused":
      return mapWidget(d, a.id, (w) => {
        const f = a.field;
        if (f.query !== undefined) {
          const queries = (w.queries ?? []).map((q, i) =>
            i === f.query ? withFields(q, { [f.field]: undefined } as Partial<DashboardQuery>) : q,
          );
          return { ...w, queries };
        }
        return withFields(w, { [f.field]: undefined } as Partial<Widget>);
      });
    case "addQuery":
      return mapWidget(d, a.id, (w) => ({ ...w, queries: [...(w.queries ?? []), { q: "" }] }));
    case "removeQuery":
      return mapWidget(d, a.id, (w) => ({ ...w, queries: (w.queries ?? []).filter((_, i) => i !== a.index) }));
    case "setQuery":
      return mapWidget(d, a.id, (w) => ({
        ...w,
        queries: (w.queries ?? []).map((q, i) => (i === a.index ? withFields(q, a.patch) : q)),
      }));
    case "addFormat":
      return mapWidget(d, a.id, (w) => ({
        ...w,
        conditional_formats: [...(w.conditional_formats ?? []), { op: ">", value: 0, color: "red" }],
      }));
    case "removeFormat":
      return mapWidget(d, a.id, (w) => {
        const rest = (w.conditional_formats ?? []).filter((_, i) => i !== a.index);
        return withFields(w, { conditional_formats: rest.length ? rest : undefined });
      });
    case "setFormat":
      return mapWidget(d, a.id, (w) => ({
        ...w,
        conditional_formats: (w.conditional_formats ?? []).map((f, i) =>
          i === a.index ? withFields(f, a.patch) : f,
        ),
      }));
  }
}

/** The columns a stored dashboard has that a definition must not. */
export const DATABASE_FIELDS = ["id", "provisioned", "created_at", "updated_at"] as const;

/**
 * A definition with the database's columns removed.
 *
 * The API splices `id`, `provisioned`, `created_at` and `updated_at` in
 * beside a definition's fields when it serves one, and refuses a definition
 * that contains them (they are not fields of one). So a stored dashboard sent
 * back as-is is a 400 — this is what makes export, copy and save work on
 * something that came from `GET`.
 */
export function definitionOf(d: Dashboard): { definition: Dashboard; dropped: string[] } {
  const out: Record<string, unknown> = { ...d };
  const dropped: string[] = [];
  for (const k of DATABASE_FIELDS) {
    if (Object.hasOwn(out, k)) {
      dropped.push(k);
      delete out[k];
    }
  }
  return { definition: out as unknown as Dashboard, dropped };
}

/** The JSON an export writes: the definition alone, two-space indented. */
export function exportDefinition(d: Dashboard): string {
  return JSON.stringify(definitionOf(d).definition, null, 2) + "\n";
}

/**
 * A copy of a definition to be saved as a new dashboard. The `uid` goes,
 * because it is the original's identity and saving a second row under it is
 * a 409; the title says it is a copy, so two rows in the picker are two rows.
 */
export function copyOf(d: Dashboard): Dashboard {
  const { definition } = definitionOf(d);
  const rest: Record<string, unknown> = { ...definition };
  delete rest.uid;
  return { ...(rest as unknown as Dashboard), title: withSuffix(definition.title, COPY_SUFFIX, MAX_TITLE_BYTES) };
}

/** The server's title limit, in UTF-8 bytes (`dashboard.MaxTitle`). */
export const MAX_TITLE_BYTES = 200;
const COPY_SUFFIX = " (copy)";

/**
 * `text + suffix`, cutting `text` so the whole fits in `max` UTF-8 bytes.
 *
 * Needed because a template instance's title is already cut to the limit by
 * the server (template.go), so a copy that only appended would be refused on
 * its first save for a title the author never typed. Cut on a code point, so
 * the result is never a broken character.
 */
export function withSuffix(text: string, suffix: string, max: number): string {
  const enc = new TextEncoder();
  const room = max - enc.encode(suffix).length;
  let out = "";
  let used = 0;
  for (const ch of text) {
    const n = enc.encode(ch).length;
    if (used + n > room) break;
    out += ch;
    used += n;
  }
  return out + suffix;
}

/** The result of reading pasted or uploaded JSON as a definition. */
export type ImportReading =
  | { kind: "empty" }
  | { kind: "notJson"; message: string }
  | { kind: "notDashboard"; problems: string[] }
  | { kind: "ok"; dashboard: Dashboard; dropped: string[] };

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

/**
 * Reads imported text. Checks only what the editor itself needs to open the
 * definition — a title, and widgets with an id, a type and a numeric layout —
 * and names every problem rather than the first. Everything else (a query
 * that does not parse, an unknown type, a field in the wrong place) is left
 * for the editor to show and the server to refuse, since both of those say
 * it better than a second validator here would.
 */
export function readImport(text: string): ImportReading {
  if (text.trim() === "") return { kind: "empty" };
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch (e) {
    return { kind: "notJson", message: e instanceof Error ? e.message : String(e) };
  }
  if (!isRecord(body)) return { kind: "notDashboard", problems: ["It is not a JSON object."] };
  const problems: string[] = [];
  if (typeof body.title !== "string") problems.push("title is missing or not a string.");
  if (!Array.isArray(body.widgets)) problems.push("widgets is missing or not a list.");
  else
    body.widgets.forEach((w: unknown, i) => {
      if (!isRecord(w)) return problems.push(`widgets[${i}] is not an object.`);
      if (typeof w.id !== "string") problems.push(`widgets[${i}]: id is missing or not a string.`);
      if (typeof w.type !== "string") problems.push(`widgets[${i}]: type is missing or not a string.`);
      const l = w.layout;
      if (!isRecord(l) || !["x", "y", "w", "h"].every((k) => typeof l[k] === "number" && Number.isFinite(l[k])))
        problems.push(`widgets[${i}]: layout needs numeric x, y, w and h.`);
      if (w.queries !== undefined && (!Array.isArray(w.queries) || !w.queries.every((q) => isRecord(q) && typeof q.q === "string")))
        problems.push(`widgets[${i}]: queries must be a list of {"q": "…"}.`);
      // Text the editor calls string methods on. A number here would not be
      // "unknown to this build", it would be a TypeError in the render.
      for (const k of ["title", "markdown"] as const)
        if (w[k] !== undefined && typeof w[k] !== "string") problems.push(`widgets[${i}]: ${k} is not a string.`);
      if (w.yaxis !== undefined && !isRecord(w.yaxis)) problems.push(`widgets[${i}]: yaxis is not an object.`);
      if (w.conditional_formats !== undefined && (!Array.isArray(w.conditional_formats) || !w.conditional_formats.every(isRecord)))
        problems.push(`widgets[${i}]: conditional_formats must be a list of objects.`);
    });
  if (body.description !== undefined && typeof body.description !== "string") problems.push("description is not a string.");
  if (body.template_vars !== undefined) {
    if (!Array.isArray(body.template_vars)) problems.push("template_vars is not a list.");
    else
      // Every consumer of a variable — the variable bar, the URL state, the
      // query editor's $ completion — reads name and tag as strings.
      body.template_vars.forEach((v: unknown, i) => {
        if (!isRecord(v)) return problems.push(`template_vars[${i}] is not an object.`);
        if (typeof v.name !== "string") problems.push(`template_vars[${i}]: name is missing or not a string.`);
        if (typeof v.tag !== "string") problems.push(`template_vars[${i}]: tag is missing or not a string.`);
        if (v.default !== undefined && typeof v.default !== "string") problems.push(`template_vars[${i}]: default is not a string.`);
      });
  }
  if (problems.length) return { kind: "notDashboard", problems };
  const { definition, dropped } = definitionOf(body as unknown as Dashboard);
  return { kind: "ok", dashboard: definition, dropped };
}

/**
 * Whether two drafts are the same definition, whatever order their keys are
 * in. Order-insensitive because removing a field and typing it back re-adds
 * the key at the end of the object: comparing raw `JSON.stringify` output
 * would then call an unchanged draft modified, arm the leave-page prompt and
 * enable Save for nothing. Array order still counts — widgets and queries are
 * in the author's order, and reordering them is a change.
 */
export function sameDefinition(a: Dashboard, b: Dashboard): boolean {
  return canonical(a) === canonical(b);
}

/** JSON with every object's keys sorted, and undefined-valued keys dropped as JSON drops them. */
function canonical(v: unknown): string {
  return JSON.stringify(v, (_k, value: unknown) =>
    isRecord(value)
      ? Object.fromEntries(Object.keys(value).sort().map((k) => [k, value[k]]))
      : value,
  );
}

/** Whether a layout is on the grid as the server counts it. */
export function layoutProblem(l: Layout): string | undefined {
  if (!(l.w > 0 && l.h > 0)) return "width and height must be positive";
  if (l.x < 0 || l.y < 0) return "position cannot be negative";
  if (l.x + l.w > COLUMNS) return `extends past column ${COLUMNS}`;
  return undefined;
}
