/**
 * The inputs for the widgets that draw from their own API: a monitor
 * summary's filter, an alert value's monitor, a service summary's
 * environment. Which one shows is the type's `feed` rule.
 */
import type { MonitorStatusName, MonitorSummaryFilter, Widget } from "../../../lib/dashboard";
import type { TypeRules } from "../../../lib/dashboardEditor";
import { useMonitorList } from "../../../lib/useMonitors";
import { Section, TextField } from "./fields";

const FILTER_STATUSES: readonly MonitorStatusName[] = ["ALERT", "WARN", "NO_DATA", "OK"];

/** A filter with its empty parts removed, or undefined when nothing is left. */
function tidyFilter(f: MonitorSummaryFilter): MonitorSummaryFilter | undefined {
  const out: MonitorSummaryFilter = {};
  if (f.tag) out.tag = f.tag;
  if (f.q) out.q = f.q;
  if (f.status && f.status.length > 0) out.status = f.status;
  return Object.keys(out).length > 0 ? out : undefined;
}

/** The feed input for a widget's type, or nothing for the query widgets. */
export function FeedFields({ widget, rules, set }: { widget: Widget; rules: TypeRules; set: (patch: Partial<Widget>) => void }) {
  switch (rules.feed) {
    case "monitor_filter":
      return <MonitorFilterFields widget={widget} set={set} />;
    case "monitor_uid":
      return <MonitorPicker widget={widget} set={set} />;
    case "env":
      return (
        <Section title="Services">
          <TextField label="Environment" placeholder="every environment" value={widget.env} onChange={(env) => set({ env })} />
          {/[,{}\n]/.test(widget.env ?? "") ? (
            <p role="note" className="text-xs text-red-700 dark:text-red-400">
              An environment cannot hold a comma, a brace or a newline.
            </p>
          ) : null}
          <p className="text-xs text-zinc-500">
            A card per service, with a status dot from the monitors tagged <code>service:&lt;name&gt;</code>.
          </p>
        </Section>
      );
    default:
      return null;
  }
}

function MonitorFilterFields({ widget, set }: { widget: Widget; set: (patch: Partial<Widget>) => void }) {
  const f = widget.monitor_filter ?? {};
  const update = (patch: Partial<MonitorSummaryFilter>) => set({ monitor_filter: tidyFilter({ ...f, ...patch }) });
  const statuses = f.status ?? [];
  return (
    <Section title="Monitors">
      <div className="grid grid-cols-2 gap-2">
        <TextField label="Tag" placeholder="e.g. app:shop" value={f.tag} onChange={(tag) => update({ tag })} />
        <TextField label="Name contains" value={f.q} onChange={(q) => update({ q })} />
      </div>
      <fieldset className="flex flex-wrap gap-3 text-sm">
        <legend className="text-xs text-zinc-500">Statuses (none ticked: every monitor)</legend>
        {FILTER_STATUSES.map((s) => (
          <label key={s} className="flex items-center gap-1">
            <input
              type="checkbox"
              checked={statuses.includes(s)}
              onChange={(e) =>
                update({ status: e.target.checked ? FILTER_STATUSES.filter((x) => x === s || statuses.includes(x)) : statuses.filter((x) => x !== s) })
              }
            />
            {s.replace("_", " ")}
          </label>
        ))}
      </fieldset>
    </Section>
  );
}

function MonitorPicker({ widget, set }: { widget: Widget; set: (patch: Partial<Widget>) => void }) {
  const list = useMonitorList({});
  const withUid = (list.data ?? []).filter((m) => m.uid);
  const missing = list.data && widget.monitor_uid && !withUid.some((m) => m.uid === widget.monitor_uid);
  return (
    <Section title="Monitor">
      <label className="flex flex-col gap-0.5 text-sm">
        <span className="text-xs text-zinc-500">Monitor (by uid)</span>
        <select
          value={widget.monitor_uid ?? ""}
          onChange={(e) => set({ monitor_uid: e.target.value })}
          className="rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900"
        >
          <option value="">Choose a monitor</option>
          {missing ? <option value={widget.monitor_uid}>{widget.monitor_uid} (no such monitor)</option> : null}
          {withUid.map((m) => (
            <option key={m.id} value={m.uid}>
              {m.name}
            </option>
          ))}
        </select>
      </label>
      {list.error ? <p role="note" className="text-xs text-red-700 dark:text-red-400">Could not list monitors: {String(list.error)}</p> : null}
      {list.data && withUid.length < list.data.length ? (
        <p className="text-xs text-zinc-500">
          {list.data.length - withUid.length} monitor(s) have no uid and cannot be chosen: a widget names its monitor by uid, so a
          shipped dashboard can name a shipped monitor.
        </p>
      ) : null}
      {(widget.monitor_uid ?? "") === "" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          An alert value needs a monitor; one without is refused on save.
        </p>
      ) : null}
    </Section>
  );
}
