/**
 * Create and edit a monitor, with a preview of what it would have done.
 *
 * Every pause in typing sends the draft to `validate`, which answers with the
 * reasons it is invalid or with the levels, the mentions and a dry run at now.
 * A valid draft is then backtested over the last day, and the chart draws what
 * that replay saw: the evaluated line (after the window's reduction, not the
 * raw query), the threshold bands, the window an evaluation at now reads, and
 * a marker at every transition the state machine made. Nothing is computed
 * here from the query text: the server that will run the monitor is the one
 * that says what it would do (lib/monitorChart.ts).
 *
 * A provisioned monitor opens read-only: the server refuses a write to one
 * (409), and the file is where it is changed. The preview still works, so the
 * page doubles as a way to see what a file's monitor does.
 */
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { definitionOf, emptyForm, formOf, previewDefinitionOf, type MonitorForm } from "../../lib/monitorForm";
import { createMonitor, updateMonitor, type Backtest, type Validation } from "../../lib/monitorsApi";
import { useDebouncedValue } from "../../lib/useDebouncedValue";
import { MONITORS_KEY, useBacktest, useMonitor, useNotifiers, useValidation } from "../../lib/useMonitors";
import { MonitorChart } from "./MonitorChart";
import { button, ErrorText, input, primary, StatusPill } from "./parts";

/** How long typing must pause before the draft is validated. */
export const VALIDATE_DEBOUNCE_MS = 400;

/** The page for a new monitor. */
export function NewMonitorPage() {
  return <MonitorEditor initial={emptyForm()} />;
}

/** The page for editing a stored monitor. */
export function EditMonitorPage() {
  const id = Number(useParams().id);
  const m = useMonitor(Number.isInteger(id) ? id : undefined);
  if (!Number.isInteger(id)) return <ErrorText>Not a monitor id.</ErrorText>;
  if (m.error) return <ErrorText>{m.error.message}</ErrorText>;
  if (!m.data) return <p className="text-sm text-zinc-500">Loading…</p>;
  // Keyed by id: opening another monitor starts a fresh draft.
  return <MonitorEditor key={id} initial={formOf(m.data)} id={id} provisioned={m.data.provisioned} />;
}

function Field({ label, hint, children }: { label: string; hint?: React.ReactNode; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-0.5 text-sm">
      <span className="text-xs text-zinc-500">{label}</span>
      {children}
      {hint ? <span className="text-xs text-zinc-500">{hint}</span> : null}
    </label>
  );
}

/** The editor itself: a draft, its validation, and its backtest. */
export function MonitorEditor({ initial, id, provisioned = false }: { initial: MonitorForm; id?: number; provisioned?: boolean }) {
  const [form, setForm] = useState(initial);
  const [baseline, setBaseline] = useState(initial);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const navigate = useNavigate();
  const client = useQueryClient();
  const set = <K extends keyof MonitorForm>(k: K, v: MonitorForm[K]) => setForm((f) => ({ ...f, [k]: v }));
  const dirty = JSON.stringify(form) !== JSON.stringify(baseline);

  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  const read = useMemo(() => definitionOf(form), [form]);
  const preview = useMemo(() => previewDefinitionOf(form), [form]);
  const asked = useDebouncedValue(preview, VALIDATE_DEBOUNCE_MS);
  const validation = useValidation(asked.ok ? asked.definition : undefined);
  const valid = validation.data?.valid === true ? validation.data : undefined;
  // Backtest only what the server said is valid, and only the draft it said
  // it about: a placeholder answer for an older draft must not start a replay
  // of the newer one.
  const backtestDef = valid && !validation.isPlaceholderData && asked.ok ? asked.definition : undefined;
  const backtest = useBacktest(backtestDef, id);
  const notifiers = useNotifiers();

  const save = async () => {
    if (!read.ok) return;
    setSaving(true);
    setSaveError(null);
    try {
      const m = id === undefined ? await createMonitor(read.definition) : await updateMonitor(id, read.definition);
      setBaseline(form);
      await client.invalidateQueries({ queryKey: [MONITORS_KEY] });
      navigate(`/monitors/${m.id}`);
    } catch (e) {
      setSaveError(e instanceof Error ? e.message : String(e));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold">{form.name || "Untitled monitor"}</h1>
          <p className="text-xs text-zinc-500">
            {id === undefined ? "New monitor, not saved yet" : `Editing monitor #${id}`}
            {dirty ? " · unsaved changes" : ""}
          </p>
        </div>
        <div className="flex items-center gap-2">
          {id !== undefined ? (
            <Link to={`/monitors/${id}`} className={button}>
              Back to monitor
            </Link>
          ) : null}
          <button type="button" className={primary} disabled={provisioned || saving || !read.ok || (id !== undefined && !dirty)}
            title={read.ok ? undefined : read.errors.join("; ")} onClick={() => void save()}>
            {saving ? "Saving…" : id === undefined ? "Create monitor" : "Save"}
          </button>
        </div>
      </header>

      {provisioned ? (
        <p className="rounded-md border border-zinc-200 p-3 text-sm dark:border-zinc-800">
          This monitor is provisioned from a file, so it is read-only here: change the file. The preview below still shows
          what an edit would do.
        </p>
      ) : null}
      {saveError ? <ErrorText>{saveError}</ErrorText> : null}

      <Preview read={preview} validation={validation.data} validating={validation.isFetching} validateError={validation.error?.message}
        backtest={backtest.data} backtestLoading={backtest.isFetching} backtestError={backtest.error?.message} />

      <section aria-label="Definition" className="grid gap-4 md:grid-cols-2">
        <div className="flex flex-col gap-3">
          <Field label="Name">
            <input className={input} value={form.name} onChange={(e) => set("name", e.target.value)} />
          </Field>
          <Field label="Type">
            <select className={input} value={form.type} onChange={(e) => set("type", e.target.value as MonitorForm["type"])}>
              <option value="metric">metric</option>
              <option value="log">log</option>
            </select>
          </Field>
          <Field
            label="Query"
            hint={
              form.type === "metric" ? (
                <>
                  <code>time_agg(last_5m):space_agg:metric{"{tags}"} by {"{tag}"} &gt; threshold</code>
                </>
              ) : (
                <code>logs("filter").rollup("count").last("5m") &gt; threshold</code>
              )
            }
          >
            <textarea aria-label="Query" rows={3} className={`${input} font-mono`} value={form.query} onChange={(e) => set("query", e.target.value)} />
          </Field>
          <Field
            label="Message"
            hint={
              <>
                Go template: <code>{"{{.Value}}"}</code>, <code>{"{{.Group}}"}</code>, <code>{"{{.Status}}"}</code>,{" "}
                <code>{"{{if .IsAlert}}…{{end}}"}</code>. <code>@name</code> sends it to that notifier
                {notifiers.data?.length ? (
                  <>
                    {" "}
                    ({notifiers.data.map((n) => `@${n.name}${n.ready ? "" : " (not ready)"}`).join(", ")})
                  </>
                ) : null}
                , every time, whatever the status.
              </>
            }
          >
            <textarea aria-label="Message" rows={4} className={`${input} font-mono`} value={form.message} onChange={(e) => set("message", e.target.value)} />
          </Field>
          <Field label="Tags" hint="Separated by commas or spaces.">
            <input aria-label="Tags" className={input} value={form.tags} onChange={(e) => set("tags", e.target.value)} placeholder="team:shop service:api" />
          </Field>
          <Field label="UID" hint="Optional stable id; provisioning files match on it.">
            <input aria-label="UID" className={input} value={form.uid} onChange={(e) => set("uid", e.target.value)} />
          </Field>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={form.enabled} onChange={(e) => set("enabled", e.target.checked)} />
            Enabled
          </label>
        </div>

        <div className="flex flex-col gap-3">
          <p className="text-xs text-zinc-500">
            The critical threshold is the number at the end of the query. Leave a box empty for its default.
          </p>
          <div className="grid grid-cols-2 gap-2">
            <Field label="Warning">
              <input aria-label="Warning" className={input} value={form.warning} onChange={(e) => set("warning", e.target.value)} />
            </Field>
            <Field label="Warning recovery">
              <input aria-label="Warning recovery" className={input} value={form.warning_recovery} onChange={(e) => set("warning_recovery", e.target.value)} />
            </Field>
            <Field label="Critical recovery" hint="Below it (for >) the alert clears: the gap is the hysteresis.">
              <input aria-label="Critical recovery" className={input} value={form.critical_recovery} onChange={(e) => set("critical_recovery", e.target.value)} />
            </Field>
            <Field label="Consecutive failures">
              <input aria-label="Consecutive failures" className={input} value={form.min_consecutive_failures} onChange={(e) => set("min_consecutive_failures", e.target.value)} />
            </Field>
            <Field label="Evaluate every (s)">
              <input aria-label="Evaluation interval" className={input} value={form.evaluation_interval_s} onChange={(e) => set("evaluation_interval_s", e.target.value)} />
            </Field>
            <Field label="Delay (s)" hint="How far behind now the window ends.">
              <input aria-label="Evaluation delay" className={input} value={form.evaluation_delay_s} onChange={(e) => set("evaluation_delay_s", e.target.value)} />
            </Field>
            <Field label="Renotify every (m)">
              <input aria-label="Renotify interval" className={input} value={form.renotify_interval_m} onChange={(e) => set("renotify_interval_m", e.target.value)} />
            </Field>
            <Field label="No data after (m)">
              <input aria-label="No-data timeframe" className={input} value={form.no_data_timeframe_m} onChange={(e) => set("no_data_timeframe_m", e.target.value)} />
            </Field>
          </div>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={form.notify_no_data} onChange={(e) => set("notify_no_data", e.target.checked)} />
            Notify when a group stops reporting (NO DATA)
          </label>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={form.require_full_window} onChange={(e) => set("require_full_window", e.target.checked)} />
            Require a full window (skip a group whose window has a gap)
          </label>
        </div>
      </section>
    </div>
  );
}

/** Why the draft is invalid, or what it would do. */
function Preview({
  read,
  validation,
  validating,
  validateError,
  backtest,
  backtestLoading,
  backtestError,
}: {
  read: ReturnType<typeof definitionOf>;
  validation: Validation | undefined;
  validating: boolean;
  validateError: string | undefined;
  backtest: Backtest | undefined;
  backtestLoading: boolean;
  backtestError: string | undefined;
}) {
  const problems = !read.ok ? read.errors : validation?.valid === false ? validation.errors : [];
  const valid = read.ok && validation?.valid === true ? validation : undefined;
  return (
    <section aria-label="Preview" className="flex flex-col gap-3 rounded-md border border-zinc-200 p-3 dark:border-zinc-800">
      <div className="flex items-center justify-between">
        <h2 className="text-sm font-semibold">Preview: the last day</h2>
        <span className="text-xs text-zinc-500">{validating || backtestLoading ? "checking…" : ""}</span>
      </div>
      {validateError ? <ErrorText>{validateError}</ErrorText> : null}
      {problems.length > 0 ? (
        <ul aria-label="Problems" className="list-disc pl-5 text-sm text-red-700 dark:text-red-400">
          {problems.map((p) => (
            <li key={p}>{p}</li>
          ))}
        </ul>
      ) : null}
      {valid ? (
        <>
          <p className="text-xs text-zinc-500">
            Reads as <code>{valid.query}</code>
            {valid.mentions.length ? <> · notifies {valid.mentions.map((m) => `@${m}`).join(", ")}</> : <> · notifies nobody (no @mention)</>}
          </p>
          {valid.eval_error ? <ErrorText>The query fails right now: {valid.eval_error}</ErrorText> : null}
          {backtestError ? <ErrorText>{backtestError}</ErrorText> : null}
          {backtest ? (
            <>
              <MonitorChart backtest={backtest} levels={valid.levels} evalWindow={valid.window} />
              <BacktestSummary backtest={backtest} />
            </>
          ) : null}
          {valid.groups?.length ? <DryRun groups={valid.groups} /> : null}
        </>
      ) : null}
    </section>
  );
}

function BacktestSummary({ backtest }: { backtest: Backtest }) {
  const transitions = backtest.events.filter((e) => e.from !== e.to);
  const notifications = backtest.events.filter((e) => e.notify).length;
  return (
    <div className="flex flex-col gap-1 text-sm">
      <p>
        Over {backtest.evaluations} evaluations it would have changed status {transitions.length} time(s) and notified{" "}
        {notifications} time(s).
      </p>
      {transitions.length > 0 ? (
        <details>
          <summary className="cursor-pointer text-xs text-zinc-500">Transitions</summary>
          <ul aria-label="Backtest transitions" className="text-xs">
            {transitions.map((e) => (
              <li key={`${e.ts}-${e.group}`}>
                {new Date(e.ts).toLocaleString()} · {e.group} · {e.from} → {e.to}
                {e.value !== null ? ` at ${e.value}` : ""}
              </li>
            ))}
          </ul>
        </details>
      ) : null}
    </div>
  );
}

function DryRun({ groups }: { groups: NonNullable<Extract<Validation, { valid: true }>["groups"]> }) {
  return (
    <details>
      <summary className="cursor-pointer text-xs text-zinc-500">Now: {groups.length} group(s)</summary>
      <table className="text-xs">
        <tbody>
          {groups.map((g) => (
            <tr key={g.group}>
              <td className="pr-2">{g.group}</td>
              <td className="pr-2 text-right">{g.value ?? "no value"}</td>
              <td>
                <StatusPill status={g.value === null ? null : g.status} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </details>
  );
}
