/**
 * TanStack Query hooks for the monitor pages.
 *
 * Keys hold what is asked, never a resolved timestamp (useMetricsApi.ts):
 * "the last day" re-resolves on each refetch. The list, states and status
 * poll, because a monitor page is looked at during an incident and a stale
 * "OK" there is worse than none.
 */
import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { useMemo } from "react";
import type { Marker } from "./annotations";
import { transitionMarkers } from "./monitorChart";
import {
  backtestMonitor,
  fetchEvents,
  fetchMonitor,
  fetchMonitors,
  fetchMonitorsStatus,
  fetchNotifiers,
  fetchStates,
  validateMonitor,
  type Backtest,
  type EventFilter,
  type GroupState,
  type MonitorDefinition,
  type MonitorEvent,
  type MonitorFilter,
  type MonitorsStatus,
  type Notifier,
  type StoredMonitor,
  type Validation,
} from "./monitorsApi";

/** How often live monitor views refetch. */
export const MONITOR_POLL_MS = 15_000;

/** Every query key this module uses starts with this, for invalidation. */
export const MONITORS_KEY = "monitors";

/** The list, filtered. */
export function useMonitorList(filter: MonitorFilter): UseQueryResult<StoredMonitor[]> {
  return useQuery({
    queryKey: [MONITORS_KEY, "list", filter],
    queryFn: ({ signal }) => fetchMonitors(filter, fetch, signal),
    refetchInterval: MONITOR_POLL_MS,
  });
}

/**
 * Whether a query key is about one monitor: its row, states, events or
 * backtest. By position, as the keys in this file are built, not by "contains
 * the number": a 86400-second span is not monitor 86400.
 */
export function isAboutMonitor(key: readonly unknown[], id: number): boolean {
  if (key[0] !== MONITORS_KEY) return false;
  switch (key[1]) {
    case "one":
    case "states":
      return key[2] === id;
    case "events":
      return typeof key[2] === "object" && key[2] !== null && (key[2] as { monitor_id?: number }).monitor_id === id;
    case "backtest":
      return key[3] === id;
    default:
      return false;
  }
}

/**
 * One monitor. `poll` is off for the editor, which seeds its draft once: a
 * poll there would only fetch answers the draft never reads. Instead it always
 * fetches on mount, so the draft starts from the server's copy rather than a
 * cached one that may predate someone else's save.
 */
export function useMonitor(id: number | undefined, poll = true): UseQueryResult<StoredMonitor> {
  return useQuery({
    queryKey: [MONITORS_KEY, "one", id],
    queryFn: ({ signal }) => fetchMonitor(id as number, fetch, signal),
    enabled: id !== undefined,
    refetchInterval: poll ? MONITOR_POLL_MS : false,
    refetchOnMount: poll ? true : "always",
  });
}

/** A monitor's group states. */
export function useMonitorStates(id: number): UseQueryResult<GroupState[]> {
  return useQuery({
    queryKey: [MONITORS_KEY, "states", id],
    queryFn: ({ signal }) => fetchStates(id, fetch, signal),
    refetchInterval: MONITOR_POLL_MS,
  });
}

/**
 * The event history. `sinceSeconds` is how far back from now, resolved at
 * fetch time; the key holds the span, not the instant.
 */
export function useMonitorEvents(
  filter: Omit<EventFilter, "from" | "to">,
  sinceSeconds: number,
): UseQueryResult<MonitorEvent[]> {
  return useQuery({
    queryKey: [MONITORS_KEY, "events", filter, sinceSeconds],
    queryFn: ({ signal }) => {
      const now = Date.now() / 1000;
      return fetchEvents({ ...filter, from: now - sinceSeconds, to: now }, fetch, signal);
    },
    refetchInterval: MONITOR_POLL_MS,
  });
}

/**
 * Markers for every monitor transition in a dashboard's window, for the
 * events overlay. Keyed by the window the server evaluated (stable per
 * answer), and only fetched while the overlay is on.
 */
export function useEventMarkers(xRange: [number, number] | undefined, enabled: boolean): Marker[] | undefined {
  const q = useQuery({
    queryKey: [MONITORS_KEY, "overlay", xRange],
    queryFn: ({ signal }) => fetchEvents({ from: xRange![0], to: xRange![1], kind: ["transition"], limit: 5000 }, fetch, signal),
    enabled: enabled && xRange !== undefined,
    placeholderData: (prev) => prev,
  });
  return useMemo(() => (enabled && q.data ? transitionMarkers(q.data) : undefined), [enabled, q.data]);
}

/** The scheduler's health and the badge counts. */
export function useMonitorsStatus(): UseQueryResult<MonitorsStatus> {
  return useQuery({
    queryKey: [MONITORS_KEY, "status"],
    queryFn: ({ signal }) => fetchMonitorsStatus(fetch, signal),
    refetchInterval: MONITOR_POLL_MS,
  });
}

/** The configured notifiers. */
export function useNotifiers(): UseQueryResult<Notifier[]> {
  return useQuery({
    queryKey: [MONITORS_KEY, "notifiers"],
    queryFn: ({ signal }) => fetchNotifiers(fetch, signal),
    staleTime: 60_000,
  });
}

/**
 * Validates a definition (the caller debounces it). Keyed by the definition's
 * JSON, so an unchanged draft is not asked about twice.
 */
export function useValidation(def: MonitorDefinition | undefined): UseQueryResult<Validation> {
  return useQuery({
    queryKey: [MONITORS_KEY, "validate", def === undefined ? null : JSON.stringify(def)],
    queryFn: ({ signal }) => validateMonitor(def as MonitorDefinition, fetch, signal),
    enabled: def !== undefined,
    placeholderData: (prev) => prev,
  });
}

/**
 * Backtests a definition over the last day. Only run for a definition the
 * server said is valid: an invalid one is a 400, and the reasons are already
 * on the page from validate.
 */
export function useBacktest(def: MonitorDefinition | undefined, id: number | undefined): UseQueryResult<Backtest> {
  return useQuery({
    queryKey: [MONITORS_KEY, "backtest", def === undefined ? null : JSON.stringify(def), id],
    queryFn: ({ signal }) => backtestMonitor(def as MonitorDefinition, { id }, fetch, signal),
    enabled: def !== undefined,
    placeholderData: (prev) => prev,
    staleTime: 60_000,
  });
}
