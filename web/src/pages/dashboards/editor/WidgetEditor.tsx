/**
 * The panel that edits one widget: every field docs/dashboards.md defines for
 * its type, and nothing it does not.
 *
 * What is offered comes from [[TYPE_RULES]], so a field appears exactly where
 * the server accepts it. What the widget *carries* is a separate question,
 * and the panel answers both: a field the type does not use is listed with
 * the server's reason and a Remove button ([[unusedFields]]), a key this
 * build does not read is listed as kept ([[unknownKeys]]), and an enum value
 * this build does not know is shown as itself ([[EnumSelect]]). None of
 * those is dropped on the author's behalf.
 */
import type { Dispatch } from "react";
import type { ConditionalFormat, Dashboard, DashboardQuery, Widget } from "../../../lib/dashboard";
import {
  DISPLAYS,
  FORMAT_COLORS,
  layoutProblem,
  leadingAggregator,
  OPS,
  readEnum,
  REDUCER_OPTIONS,
  rulesFor,
  SCALES,
  TYPE_RULES,
  unknownKeys,
  unusedFields,
  WIDGET_TYPES,
  type EditorAction,
  type TypeRules,
} from "../../../lib/dashboardEditor";
import { EnumSelect } from "../../../ui/EnumSelect";
import { NumberField } from "../../../ui/NumberField";
import { button, Section, TextField } from "./fields";
import { QueryEditor } from "./QueryEditor";

/** Props for WidgetEditor. */
export interface WidgetEditorProps {
  widget: Widget;
  dashboard: Dashboard;
  dispatch: Dispatch<EditorAction>;
  onClose: () => void;
}

const MAX_QUERIES = 10;

/** Edits one widget. */
export function WidgetEditor({ widget, dashboard, dispatch, onClose }: WidgetEditorProps) {
  const rules = rulesFor(widget.type);
  const set = (patch: Partial<Widget>) => dispatch({ type: "setWidget", id: widget.id, patch });
  const unused = unusedFields(widget);
  const extra = unknownKeys(widget, "widget");
  const problem = layoutProblem(widget.layout);

  return (
    <aside aria-label={`Edit ${widget.title ?? widget.id}`} className="flex flex-col gap-3 text-sm">
      <div className="flex items-center justify-between gap-2">
        <h2 className="font-semibold">
          Widget <code className="font-mono text-xs text-zinc-500">{widget.id}</code>
        </h2>
        <div className="flex gap-1">
          <button type="button" className={button} onClick={() => dispatch({ type: "duplicateWidget", id: widget.id })}>
            Duplicate
          </button>
          <button
            type="button"
            className={`${button} text-red-700 dark:text-red-400`}
            onClick={() => {
              dispatch({ type: "removeWidget", id: widget.id });
              onClose();
            }}
          >
            Remove
          </button>
          <button type="button" className={button} onClick={onClose} aria-label="Close widget editor">
            ×
          </button>
        </div>
      </div>

      <EnumSelect
        label="Type"
        reading={readEnum(widget.type, WIDGET_TYPES)}
        options={WIDGET_TYPES}
        optionLabel={(t) => TYPE_RULES[t].label}
        onChange={(t) => t && dispatch({ type: "setType", id: widget.id, to: t })}
      />
      <TextField label="Title" value={widget.title} onChange={(title) => set({ title })} />
      {problem ? (
        <p role="note" className="text-xs text-red-700 dark:text-red-400">
          Layout {problem}; drag it back onto the grid.
        </p>
      ) : null}

      {rules ? (
        <TypedFields widget={widget} rules={rules} dashboard={dashboard} dispatch={dispatch} set={set} />
      ) : (
        <Section title="Fields">
          <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
            {`This build does not know what a "${widget.type}" widget uses, so it cannot offer its fields. They are kept exactly as they are; choose a known type to edit them here, or edit the JSON.`}
          </p>
          <pre className="max-h-48 overflow-auto rounded bg-zinc-50 p-2 font-mono text-xs dark:bg-zinc-950">
            {JSON.stringify(widget, null, 2)}
          </pre>
        </Section>
      )}

      {unused.length ? (
        <Section title="Not used by this type">
          <p className="text-xs text-zinc-500">
            Kept from before, and refused on save until removed — the server does not ignore a field that belongs to another type.
          </p>
          <ul className="flex flex-col gap-1">
            {unused.map((f) => (
              <li key={`${f.field}:${f.query ?? ""}`} className="flex items-center justify-between gap-2 text-xs">
                <span>
                  <code className="font-mono">{f.query === undefined ? f.field : `queries[${f.query}].${f.field}`}</code> — {f.message}
                </span>
                <button type="button" className={button} onClick={() => dispatch({ type: "removeUnused", id: widget.id, field: f })}>
                  Remove
                </button>
              </li>
            ))}
          </ul>
        </Section>
      ) : null}

      {extra.length ? (
        <p role="note" className="text-xs text-zinc-500">
          Also carries {extra.map((k) => `"${k}"`).join(", ")}, which this build does not read. Kept as-is.
        </p>
      ) : null}
    </aside>
  );
}

/** The fields a known type uses, in the order a reader thinks about them. */
function TypedFields({
  widget,
  rules,
  dashboard,
  dispatch,
  set,
}: {
  widget: Widget;
  rules: TypeRules;
  dashboard: Dashboard;
  dispatch: Dispatch<EditorAction>;
  set: (patch: Partial<Widget>) => void;
}) {
  const queries = widget.queries ?? [];
  const variables = (dashboard.template_vars ?? []).map((v) => v.name.toLowerCase());
  const canAdd = rules.queries === "many" ? queries.length < MAX_QUERIES : rules.queries === "one" && queries.length === 0;
  return (
    <>
      {rules.queries !== "none" ? (
        <Section title={rules.queries === "one" ? "Query" : "Queries"}>
          {rules.queries === "one" && queries.length > 1 ? (
            <p role="note" className="text-xs text-red-700 dark:text-red-400">
              A heatmap draws one distribution; remove all but one query, or use a widget each.
            </p>
          ) : null}
          {queries.map((q, i) => (
            <QueryFields
              key={i}
              widgetId={widget.id}
              index={i}
              query={q}
              rules={rules}
              variables={variables}
              removable={queries.length > 1}
              dispatch={dispatch}
            />
          ))}
          {canAdd ? (
            <button type="button" className={`${button} self-start`} onClick={() => dispatch({ type: "addQuery", id: widget.id })}>
              Add query
            </button>
          ) : null}
        </Section>
      ) : null}

      {rules.markdown ? (
        <Section title="Text">
          <label className="flex flex-col gap-0.5">
            <span className="text-xs text-zinc-500">Markdown (shown as plain text)</span>
            <textarea
              value={widget.markdown ?? ""}
              rows={6}
              onChange={(e) => set({ markdown: e.target.value })}
              className="rounded-md border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
            />
          </label>
          {(widget.markdown ?? "").trim() === "" ? (
            <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
              A note needs text; an empty one is refused on save.
            </p>
          ) : null}
        </Section>
      ) : null}

      {rules.yaxis ? (
        <Section title="Y axis">
          <div className="grid grid-cols-2 gap-2">
            <NumberField
              label="Min"
              placeholder="fit the data"
              value={widget.yaxis?.min}
              onChange={(min) => set({ yaxis: tidy({ ...widget.yaxis, min }) })}
            />
            <NumberField
              label="Max"
              placeholder="fit the data"
              value={widget.yaxis?.max}
              onChange={(max) => set({ yaxis: tidy({ ...widget.yaxis, max }) })}
            />
            <TextField
              label="Unit (a label, not a conversion)"
              value={widget.yaxis?.unit}
              onChange={(unit) => set({ yaxis: tidy({ ...widget.yaxis, unit }) })}
            />
            <EnumSelect
              label="Scale"
              reading={readEnum(widget.yaxis?.scale, SCALES)}
              options={SCALES}
              absent={{ label: "linear (the default)" }}
              onChange={(scale) => set({ yaxis: tidy({ ...widget.yaxis, scale }) })}
            />
          </div>
          {widget.yaxis?.min !== undefined && widget.yaxis?.max !== undefined && widget.yaxis.min >= widget.yaxis.max ? (
            <p role="note" className="text-xs text-red-700 dark:text-red-400">
              Min must be below max.
            </p>
          ) : null}
          {widget.yaxis?.scale === "log" && widget.yaxis.min !== undefined && widget.yaxis.min <= 0 ? (
            <p role="note" className="text-xs text-red-700 dark:text-red-400">
              A log axis cannot start at or below zero.
            </p>
          ) : null}
          {widget.type === "heatmap" && widget.yaxis?.scale !== "log" ? (
            <p className="text-xs text-zinc-500">A heatmap&apos;s bins are geometric; a log scale is usually what you want.</p>
          ) : null}
        </Section>
      ) : null}

      {rules.precision || rules.limit ? (
        <Section title="Display">
          <div className="grid grid-cols-2 gap-2">
            {rules.precision ? (
              <NumberField
                label="Decimal places"
                placeholder="automatic"
                integer
                min={0}
                max={10}
                value={widget.precision}
                onChange={(precision) => set({ precision })}
              />
            ) : null}
            {rules.limit ? (
              <NumberField
                label="Rows"
                placeholder="10 (the default)"
                integer
                min={1}
                max={100}
                value={widget.limit}
                onChange={(limit) => set({ limit })}
              />
            ) : null}
          </div>
        </Section>
      ) : null}

      {rules.conditionalFormats ? <Formats widget={widget} dispatch={dispatch} /> : null}
    </>
  );
}

/**
 * A y-axis with no fields left is removed rather than saved as `{}`: the
 * server reads `{}` as "present", which on a type that does not use a y-axis
 * is a refusal for a setting with nothing in it.
 */
function tidy(y: Record<string, unknown>): Widget["yaxis"] {
  const out = Object.fromEntries(Object.entries(y).filter(([, v]) => v !== undefined));
  return Object.keys(out).length ? (out as Widget["yaxis"]) : undefined;
}

function QueryFields({
  widgetId,
  index,
  query,
  rules,
  variables,
  removable,
  dispatch,
}: {
  widgetId: string;
  index: number;
  query: DashboardQuery;
  rules: TypeRules;
  variables: readonly string[];
  removable: boolean;
  dispatch: Dispatch<EditorAction>;
}) {
  const set = (patch: Partial<DashboardQuery>) => dispatch({ type: "setQuery", id: widgetId, index, patch });
  const agg = leadingAggregator(query.q);
  const extra = unknownKeys(query, "query");
  return (
    <div className="flex flex-col gap-2 rounded-md border border-zinc-200 p-2 dark:border-zinc-800">
      <QueryEditor label={`Query ${index + 1}`} value={query.q} onChange={(q) => set({ q })} variables={variables} />
      {rules.dist === "required" && agg !== undefined && agg !== "dist" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          A heatmap draws a distribution: its query needs the dist aggregator, not {agg}.
        </p>
      ) : rules.dist === "refused" && agg === "dist" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          dist: answers a distribution, which only a heatmap draws; use a percentile (p50…p99) here.
        </p>
      ) : null}
      <div className="grid grid-cols-2 gap-2">
        {rules.queries === "many" ? (
          <TextField label="Name" placeholder="the series label" value={query.name} onChange={(name) => set({ name })} />
        ) : null}
        {rules.display ? (
          <EnumSelect
            label="Display"
            reading={readEnum(query.display, DISPLAYS)}
            options={DISPLAYS}
            absent={{ label: "line (the default)" }}
            onChange={(display) => set({ display })}
          />
        ) : null}
        {rules.reducer === "required" ? (
          <EnumSelect
            label="Reducer (over time)"
            reading={readEnum(query.reducer, REDUCER_OPTIONS)}
            options={REDUCER_OPTIONS}
            onChange={(reducer) => set({ reducer })}
          />
        ) : null}
      </div>
      {extra.length ? (
        <p role="note" className="text-xs text-zinc-500">
          Also carries {extra.map((k) => `"${k}"`).join(", ")}, which this build does not read. Kept as-is.
        </p>
      ) : null}
      {removable ? (
        <button
          type="button"
          className={`${button} self-start`}
          onClick={() => dispatch({ type: "removeQuery", id: widgetId, index })}
        >
          Remove query {index + 1}
        </button>
      ) : null}
    </div>
  );
}

function Formats({ widget, dispatch }: { widget: Widget; dispatch: Dispatch<EditorAction> }) {
  const formats = widget.conditional_formats ?? [];
  return (
    <Section title="Conditional formats">
      <p className="text-xs text-zinc-500">In order; the first that matches colours the value.</p>
      {formats.map((f, i) => {
        const set = (patch: Partial<ConditionalFormat>) =>
          dispatch({ type: "setFormat", id: widget.id, index: i, patch });
        return (
          <div key={i} className="grid grid-cols-[1fr_1fr_1fr_auto] items-end gap-2">
            <EnumSelect label="When value" reading={readEnum(f.op, OPS)} options={OPS} onChange={(op) => op && set({ op })} />
            <NumberField label="Than" value={f.value} onChange={(value) => set({ value })} />
            <EnumSelect
              label="Colour"
              reading={readEnum(f.color, FORMAT_COLORS)}
              options={FORMAT_COLORS.filter((c) => c !== "gray" || f.color === "gray")}
              onChange={(color) => color && set({ color })}
            />
            <button
              type="button"
              className={button}
              aria-label={`Remove format ${i + 1}`}
              onClick={() => dispatch({ type: "removeFormat", id: widget.id, index: i })}
            >
              ×
            </button>
            {typeof f.value !== "number" ? (
              <p role="note" className="col-span-4 text-xs text-red-700 dark:text-red-400">
                Needs a number to compare against — saved without one, the server compares against 0.
              </p>
            ) : null}
          </div>
        );
      })}
      <button type="button" className={`${button} self-start`} onClick={() => dispatch({ type: "addFormat", id: widget.id })}>
        Add format
      </button>
    </Section>
  );
}
