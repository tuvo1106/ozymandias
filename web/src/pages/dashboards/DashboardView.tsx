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
import {
  collectRequests,
  sharedWarnings,
} from "../../lib/dashboardQueries";
import {
  parseViewState,
  serializeViewState,
  type DashboardViewState,
} from "../../lib/dashboardState";
import { useDashboardData } from "../../lib/useDashboards";
import type { TimeRange } from "../../lib/timeRange";
import { REFRESH_INTERVAL_MS } from "../../lib/useMetricsApi";
import { TimeRangePicker } from "../metrics/TimeRangePicker";
import { DashboardGrid } from "./DashboardGrid";
import { VariableBar } from "./VariableBar";

/** Props for DashboardView. */
export interface DashboardViewProps {
  dashboard: Dashboard;
  /**
   * Distinguishes this dashboard's cursor group and its cache entries — one
   * dashboard's charts share a crosshair, and one dashboard's answers are
   * filed under its own widget ids.
   */
  syncKey: string;
  /** Rendered to the right of the title — a provisioned badge, an edit link. */
  actions?: React.ReactNode;
}

/** Draws a dashboard and the controls that scope it. */
export function DashboardView({
  dashboard,
  syncKey,
  actions,
}: DashboardViewProps) {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseViewState(params), [params]);
  const update = (patch: Partial<DashboardViewState>) =>
    setParams(serializeViewState({ ...state, ...patch }));
  const requests = useMemo(
    () => collectRequests(dashboard.widgets),
    [dashboard.widgets],
  );
  // syncKey identifies this dashboard, so it is what keeps two dashboards with
  // the same queries from sharing a cache entry keyed by widget id.
  const data = useDashboardData(
    requests,
    state,
    dashboard.template_vars,
    syncKey,
  );
  const shared = useMemo(
    () => sharedWarnings(dashboard.widgets, data.byWidget, data.sketches),
    [dashboard.widgets, data.byWidget, data.sketches],
  );
  const hidden = useMemo(() => new Set(shared), [shared]);

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
  const xRange: [number, number] | undefined = data.range
    ? [data.range.from, data.range.to]
    : undefined;

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold">{dashboard.title}</h1>
          {dashboard.description ? (
            <p className="mt-1 max-w-3xl text-sm text-zinc-500">
              {dashboard.description}
            </p>
          ) : null}
        </div>
        {actions}
      </header>

      <DashboardControls
        dashboard={dashboard}
        state={state}
        onChange={update}
      />

      {/* A failed *request* is the only thing that blanks the page: a failed
          query is one widget's problem and is drawn inside it (ADR-0017). */}
      {data.error ? (
        <p
          role="alert"
          className="rounded-md border border-red-200 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400"
        >
          {data.error.message}
        </p>
      ) : null}

      {dashboard.widgets.length === 0 ? (
        <p className="text-sm text-zinc-500">
          This dashboard has no widgets yet.
        </p>
      ) : (
        <div
          // Dimmed while what is on screen answers the *previous* window: the
          // page keeps the old picture rather than emptying, and this is what
          // says so. Not on an auto-refresh, which asks the same question
          // again and would otherwise blink every ten seconds.
          className={
            data.isRefreshing
              ? "opacity-50 transition-opacity"
              : "transition-opacity"
          }
        >
          <SharedWarnings warnings={shared} />
          <DashboardGrid
            widgets={dashboard.widgets}
            byWidget={data.byWidget}
            sketches={data.sketches}
            xRange={xRange}
            syncKey={syncKey}
            hiddenWarnings={hidden}
          />
        </div>
      )}
    </div>
  );
}

/** Props for DashboardControls. */
export interface DashboardControlsProps {
  dashboard: Dashboard;
  state: DashboardViewState;
  onChange: (patch: Partial<DashboardViewState>) => void;
}

/**
 * The bar above a dashboard: its variables, the time picker and auto-refresh.
 * Shared by the view and the editor, because the editor's preview answers the
 * same question the view does and should be scoped the same way.
 */
export function DashboardControls({
  dashboard,
  state,
  onChange: update,
}: DashboardControlsProps) {
  const metric = useMemo(
    () => firstMetric(dashboard.widgets) ?? "",
    [dashboard.widgets],
  );
  const canRefresh = state.range.kind === "relative";
  return (
      <div className="flex flex-wrap items-center gap-4 rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
      <VariableBar
        variables={dashboard.template_vars ?? []}
        state={state}
        metric={metric}
        onChange={(name, value) =>
          update({ vars: { ...state.vars, [name]: value } })
        }
      />
      <TimeRangePicker
        key={JSON.stringify(state.range)}
        range={state.range}
        onChange={(range: TimeRange) => update({ range })}
      />
      <label
        className="flex items-center gap-2 text-sm"
        title={
          canRefresh
            ? undefined
            : "A fixed time window doesn't change, so there is nothing to refresh."
        }
      >
        <input
          type="checkbox"
          checked={state.live && canRefresh}
          disabled={!canRefresh}
          onChange={(e) => update({ live: e.target.checked })}
        />
        Auto-refresh every {REFRESH_INTERVAL_MS / 1000}s
      </label>
    </div>
  );
}

/**
 * The warnings every answering widget shares, said once above the grid. See
 * [[sharedWarnings]] for when a warning qualifies — and why nothing does
 * while any widget is still waiting.
 */
export function SharedWarnings({ warnings }: { warnings: readonly string[] }) {
  if (warnings.length === 0) return null;
  return (
    <ul
      aria-label="Warnings on every widget"
      className="mb-3 space-y-0.5 rounded-md border border-amber-200 px-3 py-2 text-xs text-amber-700 dark:border-amber-900 dark:text-amber-500"
    >
      {warnings.map((w) => (
        <li key={w}>Every widget: {w}</li>
      ))}
    </ul>
  );
}
