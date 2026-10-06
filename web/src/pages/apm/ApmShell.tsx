import { useCallback, useMemo, type ReactNode } from "react";
import { NavLink, useSearchParams } from "react-router";
import { DEFAULT_APM_STATE, parseApmState, serializeApmState, type ApmState } from "../../lib/apmState";
import { TimeRangePicker } from "../metrics/TimeRangePicker";

/** Shared button style of the APM pages (the Log Explorer's, so the sections look like one product). */
export const BTN = "rounded-md border border-zinc-300 px-2 py-1 text-sm hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800";

/**
 * The APM view state, read from and written to the URL. `update` merges a
 * patch and pushes a history entry, so the back button undoes a filter.
 * The trace view does not use it: its URL holds the selected span and tab,
 * not a search.
 */
export function useApmState(): { state: ApmState; update: (patch: Partial<ApmState>) => void; params: URLSearchParams } {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseApmState(params), [params]);
  const update = useCallback(
    (patch: Partial<ApmState>) => setParams(serializeApmState({ ...state, ...patch }), { replace: false }),
    [state, setParams],
  );
  return { state, update, params };
}

/** The env and window of a state as a query string, for tab links: tabs share them, not the filters. */
export function scopeSearch(state: ApmState): string {
  const s = serializeApmState({ ...DEFAULT_APM_STATE, env: state.env, range: state.range }).toString();
  return s ? `?${s}` : "";
}

const tab = ({ isActive }: { isActive: boolean }) =>
  `rounded-md px-3 py-1 text-sm ${isActive ? "bg-violet-100 font-medium text-violet-900 dark:bg-violet-500/15 dark:text-violet-200" : "text-zinc-600 hover:bg-zinc-100 dark:text-zinc-400 dark:hover:bg-zinc-800"}`;

/** Props for ApmHeader. */
export interface ApmHeaderProps {
  title: ReactNode;
  state: ApmState;
  update: (patch: Partial<ApmState>) => void;
  /** Extra controls after the range picker (Refresh, ...). */
  children?: ReactNode;
}

/**
 * The strip every APM page starts with: its title, the section tabs, the env
 * filter and the time range. Tabs carry env and range along, so moving from
 * the service list to the map keeps the question the same.
 */
export function ApmHeader({ title, state, update, children }: ApmHeaderProps) {
  const search = scopeSearch(state);
  return (
    <div className="flex flex-wrap items-center gap-2">
      <h1 className="mr-2 text-xl font-semibold">{title}</h1>
      <nav aria-label="APM" className="flex gap-1">
        <NavLink to={{ pathname: "/apm", search }} end className={tab}>
          Services
        </NavLink>
        <NavLink to={{ pathname: "/apm/traces", search }} className={tab}>
          Traces
        </NavLink>
        <NavLink to={{ pathname: "/apm/map", search }} className={tab}>
          Service map
        </NavLink>
      </nav>
      <span className="ml-auto flex flex-wrap items-center gap-2">
        <label className="flex items-center gap-1 text-sm text-zinc-500">
          Env
          <input
            key={state.env}
            defaultValue={state.env}
            placeholder="all"
            aria-label="Environment"
            className="w-24 rounded-md border border-zinc-300 bg-transparent px-2 py-1 text-zinc-900 dark:border-zinc-700 dark:text-zinc-100"
            onKeyDown={(e) => e.key === "Enter" && update({ env: e.currentTarget.value.trim() })}
            onBlur={(e) => e.currentTarget.value.trim() !== state.env && update({ env: e.currentTarget.value.trim() })}
          />
        </label>
        <TimeRangePicker key={JSON.stringify(state.range)} range={state.range} onChange={(range) => update({ range })} />
        {children}
      </span>
    </div>
  );
}

/** An error banner in the pages' shared style. */
export function ErrorNote({ error }: { error: Error }) {
  return (
    <p role="alert" className="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-950 dark:text-red-300">
      {error.message}
    </p>
  );
}
