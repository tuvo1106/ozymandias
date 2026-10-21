/**
 * The two places the shell shows monitor health: a badge on the Monitors nav
 * item counting triggered monitors, and a banner across every page when
 * evaluation has stopped.
 *
 * The banner is the self-watchdog's voice (docs/api.md#get-apiv1monitorsstatus):
 * when no monitor has evaluated for a while, nothing will alert, and the one
 * place that can say so is a page someone is already looking at. A failed
 * status request shows nothing: ozyd being unreachable is the home page's to
 * report, and a banner that says "unknown" on every page would be noise.
 */
import { formatUptime } from "../lib/health";
import { useMonitorsStatus } from "../lib/useMonitors";

/** Triggered monitors: those whose worst group is ALERT or NO_DATA. */
export function triggered(byStatus: Partial<Record<string, number>>): number {
  return (byStatus.ALERT ?? 0) + (byStatus.NO_DATA ?? 0);
}

/** The nav badge; nothing when nothing is triggered. */
export function MonitorsBadge() {
  const s = useMonitorsStatus();
  const n = s.data ? triggered(s.data.monitors_by_status) : 0;
  if (n === 0) return null;
  const parts = Object.entries(s.data?.monitors_by_status ?? {}).map(([k, v]) => `${v} ${k}`);
  return (
    <span
      aria-label={`${n} triggered`}
      title={parts.join(", ")}
      className="rounded-full bg-red-600 px-1.5 text-xs font-medium text-white"
    >
      {n}
    </span>
  );
}

/** The banner when evaluation has stopped. */
export function StallBanner() {
  const s = useMonitorsStatus();
  if (!s.data?.stalled) return null;
  const why = s.data.loading
    ? "the scheduler cannot read its monitors from the database and is retrying"
    : `no monitor has completed an evaluation for ${formatUptime(s.data.last_eval_age_seconds)}`;
  return (
    <div role="alert" className="border-b border-red-300 bg-red-50 px-8 py-2 text-sm text-red-800 dark:border-red-900 dark:bg-red-950 dark:text-red-200">
      Monitoring is down: {why}. Nothing will alert until it recovers; ozyd&apos;s log says why.
    </div>
  );
}
