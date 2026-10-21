/**
 * Pieces the monitor pages share: the status pill, the group counts and the
 * status timeline. The chart is in MonitorChart.tsx, so the list page does
 * not load uPlot.
 */
import { STATUS_COLOR } from "../../lib/monitorChart";
import { STATUSES, type Status, type StoredMonitor } from "../../lib/monitorsApi";
import type { Segment } from "../../lib/monitorView";

const PILL: Record<Status, string> = {
  OK: "bg-green-100 text-green-800 dark:bg-green-500/15 dark:text-green-300",
  WARN: "bg-amber-100 text-amber-800 dark:bg-amber-500/15 dark:text-amber-300",
  ALERT: "bg-red-100 text-red-800 dark:bg-red-500/15 dark:text-red-300",
  NO_DATA: "bg-zinc-200 text-zinc-700 dark:bg-zinc-700/40 dark:text-zinc-300",
};

/**
 * A status as a pill. A null status is shown, never hidden, and never as OK:
 * the reason it is null is what the person needs to know.
 */
export function StatusPill({ status, monitor }: { status: Status | null; monitor?: StoredMonitor }) {
  const base = "inline-block rounded px-1.5 py-0.5 text-xs font-medium";
  if (status) return <span className={`${base} ${PILL[status]}`}>{status.replace("_", " ")}</span>;
  const why = monitor?.problem
    ? { label: "not running", title: `Skipped by the scheduler: ${monitor.problem}` }
    : monitor && !monitor.enabled
      ? { label: "disabled", title: "Disabled: not evaluated" }
      : { label: "no data yet", title: "No group has had data yet, so there is nothing to judge" };
  return (
    <span title={why.title} className={`${base} border border-dashed border-zinc-300 text-zinc-500 dark:border-zinc-700`}>
      {why.label}
    </span>
  );
}

/** How many groups are in each status, worst first; nothing for none. */
export function GroupCounts({ groups }: { groups: StoredMonitor["groups"] }) {
  const shown = [...STATUSES].reverse().filter((s) => (groups[s] ?? 0) > 0);
  if (shown.length === 0) return null;
  return (
    <span className="flex gap-1 text-xs" aria-label="groups by status">
      {shown.map((s) => (
        <span key={s} className={`rounded px-1 ${PILL[s]}`} title={`${groups[s]} group(s) ${s}`}>
          {groups[s]} {s.replace("_", " ")}
        </span>
      ))}
    </span>
  );
}

/** One group's status over a range, as coloured stretches. */
export function TimelineBar({ segments, from, to }: { segments: Segment[]; from: number; to: number }) {
  const span = to - from || 1;
  return (
    <div className="relative h-4 w-full overflow-hidden rounded bg-zinc-100 dark:bg-zinc-800" role="img"
      aria-label={segments.map((s) => s.status ?? "unknown").join(", ")}>
      {segments.map((s) => (
        <div
          key={s.from}
          className="absolute top-0 h-full"
          title={`${s.status ?? "unknown"}: ${new Date(s.from * 1000).toLocaleString()} – ${new Date(s.to * 1000).toLocaleString()}`}
          style={{
            left: `${((s.from - from) / span) * 100}%`,
            width: `${((s.to - s.from) / span) * 100}%`,
            background: s.status ? STATUS_COLOR[s.status] : "transparent",
          }}
        />
      ))}
    </div>
  );
}

/** The editor's small secondary button. */
export const button =
  "rounded-md border border-zinc-300 px-2 py-1 text-xs hover:bg-zinc-100 disabled:opacity-50 dark:border-zinc-700 dark:hover:bg-zinc-800";

/** The primary button. */
export const primary =
  "rounded-md bg-violet-600 px-3 py-1 text-sm font-medium text-white hover:bg-violet-700 disabled:opacity-50";

/** Text inputs. */
export const input = "rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900";

/** An error sentence from the server, shown as is. */
export function ErrorText({ children }: { children: React.ReactNode }) {
  return (
    <p role="alert" className="text-sm text-red-700 dark:text-red-400">
      {children}
    </p>
  );
}
