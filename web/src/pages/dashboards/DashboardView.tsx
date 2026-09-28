/**
 * One dashboard, drawn.
 *
 * The page owns three things and delegates the rest: the URL (which holds the
 * time range, the live flag and every variable), the one batch request its
 * widgets are drawn from, and the chrome around the grid. It is deliberately
 * the *only* place that fetches — a widget that fetched for itself would
 * defeat the shared selection the batch endpoint exists for (ADR-0018), and
 * twelve widgets polling independently is twelve times the load for the same
 * picture.
 */
import { useMemo } from "react";
import { useSearchParams } from "react-router";
import type { Dashboard } from "../../lib/dashboard";
import { firstMetric } from "../../lib/dashboard";
import { collectRequests } from "../../lib/dashboardQueries";
import { parseViewState, serializeViewState, type DashboardViewState } from "../../lib/dashboardState";
import { useDashboardData } from "../../lib/useDashboards";
import type { TimeRange } from "../../lib/timeRange";
import { REFRESH_INTERVAL_MS } from "../../lib/useMetricsApi";
import { TimeRangePicker } from "../metrics/TimeRangePicker";
import { DashboardGrid } from "./DashboardGrid";
import { VariableBar } from "./VariableBar";

/** Props for DashboardView. */
export interface DashboardViewProps {
  dashboard: Dashboard;
  /** Distinguishes this dashboard's cursor group and cache entries. */
  syncKey: string;
  /** Rendered to the right of the title — a provisioned badge, an edit link. */
  actions?: React.ReactNode;
}

/** Draws a dashboard and the controls that scope it. */
export function DashboardView({ dashboard, syncKey, actions }: DashboardViewProps) {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseViewState(params), [params]);
  const update = (patch: Partial<DashboardViewState>) => setParams(serializeViewState({ ...state, ...patch }));
  const requests = useMemo(() => collectRequests(dashboard.widgets), [dashboard.widgets]);
  const data = useDashboardData(requests, state, dashboard.template_vars);
  const metric = useMemo(() => firstMetric(dashboard.widgets) ?? "", [dashboard.widgets]);

  // The x-axis covers the window the *server evaluated*, not the one the data
  // happens to span — otherwise a series that stopped reporting an hour ago
  // would draw as though it were still current, with the axis shrunk around
  // it.
  //
  // From the answer rather than from the clock: resolving a relative range
  // here would be reading the time during a render, which makes the axis a
  // different number on every re-render and is what `react-hooks/purity`
  // objects to. Until the first answer lands there is no axis and no data to
  // put on one, so undefined is the honest value.
  const xRange: [number, number] | undefined = data.range ? [data.range.from, data.range.to] : undefined;
  const canRefresh = state.range.kind === "relative";

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold">{dashboard.title}</h1>
          {dashboard.description ? <p className="mt-1 max-w-3xl text-sm text-zinc-500">{dashboard.description}</p> : null}
        </div>
        {actions}
      </header>

      <div className="flex flex-wrap items-center gap-4 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
        <VariableBar
          variables={dashboard.template_vars ?? []}
          state={state}
          metric={metric}
          onChange={(name, value) => update({ vars: { ...state.vars, [name]: value } })}
        />
        <TimeRangePicker
          key={JSON.stringify(state.range)}
          range={state.range}
          onChange={(range: TimeRange) => update({ range })}
        />
        <label
          className="flex items-center gap-2 text-sm"
          title={canRefresh ? undefined : "A fixed time window doesn't change, so there is nothing to refresh."}
        >
          <input type="checkbox" checked={state.live && canRefresh} disabled={!canRefresh} onChange={(e) => update({ live: e.target.checked })} />
          Auto-refresh every {REFRESH_INTERVAL_MS / 1000}s
        </label>
      </div>

      {/* A failed *request* is the only thing that blanks the page: a failed
          query is one widget's problem and is drawn inside it (ADR-0017). */}
      {data.error ? (
        <p role="alert" className="rounded-md border border-red-200 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400">
          {data.error.message}
        </p>
      ) : null}

      {dashboard.widgets.length === 0 ? (
        <p className="text-sm text-zinc-500">This dashboard has no widgets yet.</p>
      ) : (
        <DashboardGrid widgets={dashboard.widgets} byWidget={data.byWidget} sketches={data.sketches} xRange={xRange} syncKey={syncKey} />
      )}
    </div>
  );
}
