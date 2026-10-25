/**
 * The widgets that draw from their own API rather than from metric queries:
 * the monitor list (monitor_summary), one monitor (alert_value) and the APM
 * service table (service_summary).
 *
 * Each fetches for itself, through the same hooks the Monitors and APM pages
 * use, so a dashboard shows what those pages would and the caches are shared.
 * And each says which kind of nothing it has: no monitor matches, nothing is
 * triggered, no monitor has that uid, no service handled a request. A blank
 * square on Home reads as "all is well" whichever of those it is.
 */
import { Link } from "react-router";
import type { RedRow } from "../../lib/apmApi";
import { DEFAULT_APM_STATE } from "../../lib/apmState";
import { DEFAULT_SUMMARY_LIMIT, listParams, monitorByUid, monitorsLink, selectMonitors, serviceEnvStatus } from "../../lib/feedWidgets";
import type { MonitorStatusName } from "../../lib/dashboard";
import type { Status } from "../../lib/monitorsApi";
import type { TimeRange } from "../../lib/timeRange";
import { useServiceTable } from "../../lib/useApmQueries";
import { useMonitorList } from "../../lib/useMonitors";
import { scopeSearch } from "../apm/ApmShell";
import { Sparkline } from "../apm/Sparkline";
import { GroupCounts, StatusPill } from "../monitors/parts";
import { WidgetFrame } from "./WidgetFrame";
import type { WidgetProps } from "./widgets";

/** The range a widget uses when the page gives it none (a preview, a test). */
const FALLBACK_RANGE: TimeRange = { kind: "relative", preset: "1h" };

function errorText(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}

/**
 * Monitor statuses are now, whatever window the page shows: the monitor API
 * keeps current state, not a history of it. On a fixed window in the past,
 * that is said, so "nothing is alerting" is not read as "nothing alerted then".
 */
function nowWarning(range: TimeRange | undefined): string[] {
  return range?.kind === "absolute" ? ["Monitor status is as of now, not the chosen window."] : [];
}

/** Monitors matching the widget's filter, worst first. */
export function MonitorSummaryWidget({ widget, range }: WidgetProps) {
  const filter = widget.monitor_filter;
  const list = useMonitorList(listParams(filter));
  if (list.error && !list.data) return <WidgetFrame title={widget.title} error={errorText(list.error)} />;
  if (!list.data) return <WidgetFrame title={widget.title} />;
  const { rows, more } = selectMonitors(list.data, filter, widget.limit);
  const statuses = filter?.status ?? [];
  if (rows.length === 0) {
    // Two different answers: nothing matches the tag and name at all, or
    // things match and none is in the statuses asked for.
    const msg =
      list.data.length === 0
        ? "No monitor matches this widget's filter."
        : statuses.length > 0
          ? `Nothing is ${statuses.map(statusWord).join(", ")}.`
          : "No monitor matches this widget's filter.";
    return (
      <WidgetFrame title={widget.title} warnings={nowWarning(range)}>
        <p className="text-sm text-zinc-500">{msg}</p>
      </WidgetFrame>
    );
  }
  return (
    <WidgetFrame title={widget.title} warnings={nowWarning(range)}>
      <ul className="min-h-0 flex-1 divide-y divide-zinc-100 overflow-auto text-sm dark:divide-zinc-800">
        {rows.map((m) => (
          <li key={m.id} className="flex items-center gap-2 py-1">
            <StatusPill status={m.status} monitor={m} />
            <Link to={`/monitors/${m.id}`} className="min-w-0 flex-1 truncate hover:underline">
              {m.name}
            </Link>
            <GroupCounts groups={m.groups} />
          </li>
        ))}
      </ul>
      {more > 0 && (
        <p className="mt-1 shrink-0 text-xs text-zinc-500">
          and {more} more · <Link to={monitorsLink(filter)} className="underline">all of them</Link>
        </p>
      )}
    </WidgetFrame>
  );
}

function statusWord(s: MonitorStatusName): string {
  return s === "NO_DATA" ? "without data" : s === "ALERT" ? "alerting" : s === "WARN" ? "warning" : "OK";
}

/** One monitor's worst status and its value, as a big number. */
export function AlertValueWidget({ widget, range }: WidgetProps) {
  // The list with group states: one request, and a disabled or skipped
  // monitor comes with none, so its last stored value is never shown as now.
  const list = useMonitorList({ states: "1" });
  const m = list.data ? monitorByUid(list.data, widget.monitor_uid) : undefined;
  if (list.error && !list.data) return <WidgetFrame title={widget.title} error={errorText(list.error)} />;
  if (!list.data) return <WidgetFrame title={widget.title} />;
  if (!m) {
    return (
      <WidgetFrame
        title={widget.title}
        error={widget.monitor_uid ? `No monitor has the uid "${widget.monitor_uid}".` : "Choose a monitor for this widget."}
      />
    );
  }
  // Worst first (docs/api.md), so the first group is the one the status is.
  const worst = m.group_states?.[0];
  const value = worst?.value;
  return (
    <WidgetFrame title={widget.title ?? m.name} warnings={nowWarning(range)}>
      <Link to={`/monitors/${m.id}`} className="flex flex-1 flex-col items-center justify-center gap-1">
        <span className={`text-3xl font-semibold tabular-nums ${VALUE_COLOR[m.status ?? "none"]}`}>
          {value === undefined || value === null ? "—" : formatValue(value)}
        </span>
        <span className="flex items-center gap-1 text-xs text-zinc-500">
          <StatusPill status={m.status} monitor={m} />
          {worst && worst.group !== "*" ? <span className="truncate">{worst.group}</span> : null}
        </span>
      </Link>
    </WidgetFrame>
  );
}

const VALUE_COLOR: Record<Status | "none", string> = {
  OK: "text-emerald-700 dark:text-emerald-400",
  WARN: "text-amber-600 dark:text-amber-400",
  ALERT: "text-red-600 dark:text-red-400",
  NO_DATA: "text-zinc-500",
  none: "text-zinc-500",
};

function formatValue(v: number): string {
  const abs = Math.abs(v);
  return abs >= 100 || Number.isInteger(v) ? Math.round(v).toLocaleString() : v.toPrecision(3);
}

/** A card per service and entry-span kind, with a status dot from its monitors. */
export function ServiceSummaryWidget({ widget, range }: WidgetProps) {
  const env = widget.env ?? "";
  const r = range ?? FALLBACK_RANGE;
  const services = useServiceTable(env, r, { refetchInterval: 60_000 });
  // Every monitor with its groups in one request: a card is one env, and a
  // monitor grouped by env speaks for each env separately (serviceEnvStatus).
  const monitors = useMonitorList({ states: "1" });
  if (services.error && !services.data) return <WidgetFrame title={widget.title} error={errorText(services.error)} />;
  if (!services.data) return <WidgetFrame title={widget.title} />;
  // The server sorts busiest first, then by name (GET /api/v1/services).
  const rows = services.data.services;
  if (rows.length === 0) {
    return (
      <WidgetFrame title={widget.title}>
        <p className="text-sm text-zinc-500">
          No service handled a request in this window{env ? ` in env “${env}”` : ""}. Services appear once an instrumented app does.
        </p>
      </WidgetFrame>
    );
  }
  const limit = widget.limit && widget.limit > 0 ? widget.limit : DEFAULT_SUMMARY_LIMIT;
  const ms = monitors.data;
  const warnings = [
    ...(monitors.error && !ms ? ["Monitor statuses could not be loaded, so the cards have no status dots."] : []),
    ...(ms ? nowWarning(range) : []),
  ];
  return (
    <WidgetFrame title={widget.title} warnings={warnings}>
      <ul className="grid min-h-0 flex-1 auto-rows-min grid-cols-[repeat(auto-fill,minmax(12rem,1fr))] gap-2 overflow-auto">
        {rows.slice(0, limit).map((row) => (
          <ServiceCard
            key={`${row.service}/${row.env ?? ""}/${row.name}`}
            row={row}
            range={r}
            status={ms ? serviceEnvStatus(ms, row.service, row.env) : undefined}
            known={ms !== undefined}
          />
        ))}
      </ul>
      {rows.length > limit && <p className="mt-1 shrink-0 text-xs text-zinc-500">and {rows.length - limit} more in APM</p>}
    </WidgetFrame>
  );
}

const DOT: Record<Status, string> = {
  OK: "bg-emerald-500",
  WARN: "bg-amber-500",
  ALERT: "bg-red-500",
  NO_DATA: "bg-zinc-400",
};

/**
 * One service's card. The dot says what its monitors say: no dot and "no
 * monitors" when none is tagged with the service, a hollow dot when they have
 * no status yet, never green by default.
 */
function ServiceCard({ row, range, status, known }: { row: RedRow; range: TimeRange; status: Status | null | undefined; known: boolean }) {
  const dot =
    !known ? null : status === undefined ? (
      <span className="text-[10px] text-zinc-500">no monitors</span>
    ) : status === null ? (
      <span title="Its monitors have no group for this environment, or none is running" className="inline-block h-2 w-2 rounded-full border border-zinc-400" />
    ) : (
      <span title={`Monitors: ${status.replace("_", " ")}`} className={`inline-block h-2 w-2 rounded-full ${DOT[status]}`} />
    );
  return (
    <li className="rounded-md border border-zinc-200 p-2 text-sm dark:border-zinc-800">
      <div className="flex items-center gap-1.5">
        <Link
          to={{ pathname: `/apm/services/${encodeURIComponent(row.service)}`, search: scopeSearch({ ...DEFAULT_APM_STATE, env: row.env ?? "", range }) }}
          className="min-w-0 flex-1 truncate font-medium hover:underline">
          {row.service}
        </Link>
        {dot}
      </div>
      <p className="truncate text-xs text-zinc-500">{row.name}</p>
      <dl className="mt-1 grid grid-cols-3 gap-1 text-xs tabular-nums">
        <div>
          <dt className="text-zinc-500">req/s</dt>
          <dd>{row.requests_per_second.toFixed(row.requests_per_second < 10 ? 2 : 0)}</dd>
        </div>
        <div>
          <dt className="text-zinc-500">errors</dt>
          <dd className={row.error_pct > 0 ? "text-red-600 dark:text-red-400" : ""}>{row.error_pct.toFixed(1)}%</dd>
        </div>
        <div>
          <dt className="text-zinc-500">p95</dt>
          <dd>{row.p95_ms === null ? "—" : `${row.p95_ms.toFixed(row.p95_ms < 10 ? 1 : 0)} ms`}</dd>
        </div>
      </dl>
      <div className="mt-1">
        <Sparkline values={row.sparkline} label={`${row.service} requests`} width={160} height={20} />
      </div>
    </li>
  );
}
