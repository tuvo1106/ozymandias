/**
 * The dashboard's own fields: title, description, uid, and its template
 * variables — the panel the editor shows when no widget is selected.
 */
import type { Dispatch } from "react";
import type { Dashboard, TemplateVar } from "../../../lib/dashboard";
import { unknownKeys, type EditorAction } from "../../../lib/dashboardEditor";
import { button, Section, TextField } from "./fields";

/** Props for DashboardSettings. */
export interface DashboardSettingsProps {
  dashboard: Dashboard;
  dispatch: Dispatch<EditorAction>;
}

const MAX_VARS = 10;

/** Edits the dashboard's title, description, uid and variables. */
export function DashboardSettings({ dashboard, dispatch }: DashboardSettingsProps) {
  const vars = dashboard.template_vars ?? [];
  const setVars = (next: TemplateVar[]) =>
    dispatch({ type: "setMeta", patch: { template_vars: next.length ? next : undefined } });
  const setVar = (i: number, patch: Partial<TemplateVar>) =>
    setVars(
      vars.map((v, j) => {
        if (j !== i) return v;
        const out: Record<string, unknown> = { ...v, ...patch };
        if (out.default === undefined) delete out.default;
        return out as unknown as TemplateVar;
      }),
    );
  const extra = unknownKeys(dashboard, "dashboard");
  return (
    <aside aria-label="Dashboard settings" className="flex flex-col gap-3 text-sm">
      <h2 className="font-semibold">Dashboard</h2>
      <TextField label="Title" value={dashboard.title} onChange={(title) => dispatch({ type: "setMeta", patch: { title: title ?? "" } })} />
      {dashboard.title.trim() === "" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          A title is required.
        </p>
      ) : null}
      <label className="flex flex-col gap-0.5">
        <span className="text-xs text-zinc-500">Description</span>
        <textarea
          rows={2}
          value={dashboard.description ?? ""}
          onChange={(e) => dispatch({ type: "setMeta", patch: { description: e.target.value || undefined } })}
          className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
        />
      </label>
      <TextField
        label="uid (a stable name for provisioning; optional)"
        value={dashboard.uid}
        onChange={(uid) => dispatch({ type: "setMeta", patch: { uid } })}
      />
      {dashboard.template ? (
        <p role="note" className="text-xs text-zinc-500">
          This is a template: it is shown once per service, never as itself, and needs a <code>service</code> variable.
        </p>
      ) : null}

      <Section title="Template variables">
        <p className="text-xs text-zinc-500">
          Queries write <code>$name</code>; the selector offers values of <em>tag</em>. A blank default, or <code>*</code>, is every value.
        </p>
        {vars.map((v, i) => (
          <div key={i} className="grid grid-cols-[1fr_1fr_1fr_auto] items-end gap-2">
            <TextField label="Name" value={v.name} onChange={(name) => setVar(i, { name: name ?? "" })} />
            <TextField label="Tag" value={v.tag} onChange={(tag) => setVar(i, { tag: tag ?? "" })} />
            <TextField label="Default" placeholder="*" value={v.default} onChange={(d) => setVar(i, { default: d })} />
            <button type="button" className={button} aria-label={`Remove variable ${v.name || i + 1}`} onClick={() => setVars(vars.filter((_, j) => j !== i))}>
              ×
            </button>
          </div>
        ))}
        {vars.length < MAX_VARS ? (
          <button type="button" className={`${button} self-start`} onClick={() => setVars([...vars, { name: "", tag: "" }])}>
            Add variable
          </button>
        ) : null}
      </Section>

      {extra.length ? (
        <p role="note" className="text-xs text-zinc-500">
          Also carries {extra.map((k) => `"${k}"`).join(", ")}, which this build does not read. Kept as-is.
        </p>
      ) : null}
    </aside>
  );
}
