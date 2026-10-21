/**
 * What the monitor editor and detail page draw: the evaluated line from a
 * backtest, the threshold bands from the validated levels, the window an
 * evaluation reads, and a marker at every transition.
 *
 * Everything drawn comes from the server's answer, never from re-reading the
 * query here. The bands are `levels` (thresholds with every default applied),
 * the line is what each evaluation compared (`series`), the markers are what
 * the state machine did (`events`). A second, client-side reading of the
 * grammar would be a second source of truth for "would this have fired", which
 * is the one question the editor must not get wrong.
 */
import type { AlignedData } from "./chartData";
import type { Band, ChartAnnotations, HLine, Marker } from "./annotations";
import type { Backtest, BacktestEvent, Levels, MonitorEvent, Status } from "./monitorsApi";

/** A status's colour, for pills, bands and markers. */
export const STATUS_COLOR: Record<Status, string> = {
  OK: "#16a34a",
  WARN: "#d97706",
  ALERT: "#dc2626",
  NO_DATA: "#71717a",
};

/** With alpha, for fills: `#rrggbb` + `aa`. */
function fill(hex: string, alpha: number): string {
  return hex + Math.round(alpha * 255).toString(16).padStart(2, "0");
}

/** Whether a breach is above the threshold (`>`, `>=`) rather than below. */
export function breachesAbove(op: Levels["op"]): boolean {
  return op === ">" || op === ">=";
}

/**
 * The bands for a monitor's levels: ALERT from the critical threshold out to
 * the edge on the breaching side, WARN between warning and critical. Recovery
 * levels are dashed lines, drawn only where they differ from their threshold
 * (the default recovery *is* the threshold, and a dashed line on top of a band
 * edge says nothing).
 */
export function levelAnnotations(levels: Levels): Pick<ChartAnnotations, "bands" | "hlines" | "yInclude"> {
  const above = breachesAbove(levels.op);
  const edge = above ? Infinity : -Infinity;
  const bands: Band[] = [{ from: levels.critical, to: edge, color: fill(STATUS_COLOR.ALERT, 0.12) }];
  const hlines: HLine[] = [{ y: levels.critical, color: STATUS_COLOR.ALERT }];
  const yInclude = [levels.critical];
  if (levels.warning !== undefined) {
    bands.push({ from: levels.warning, to: levels.critical, color: fill(STATUS_COLOR.WARN, 0.12) });
    hlines.push({ y: levels.warning, color: STATUS_COLOR.WARN });
    yInclude.push(levels.warning);
    if (levels.warning_recovery !== undefined && levels.warning_recovery !== levels.warning) {
      hlines.push({ y: levels.warning_recovery, color: STATUS_COLOR.WARN, dash: true });
      yInclude.push(levels.warning_recovery);
    }
  }
  if (levels.critical_recovery !== levels.critical) {
    hlines.push({ y: levels.critical_recovery, color: STATUS_COLOR.ALERT, dash: true });
    yInclude.push(levels.critical_recovery);
  }
  return { bands, hlines, yInclude };
}

/** A marker per transition, coloured by the status it went to. */
export function transitionMarkers(events: readonly (BacktestEvent | MonitorEvent)[]): Marker[] {
  const out: Marker[] = [];
  for (const e of events) {
    if ("kind" in e && e.kind !== "transition") continue;
    const to = e.to as Status | undefined;
    if (!to || e.from === e.to) continue; // a renotify is not a transition
    out.push({ x: Date.parse(e.ts) / 1000, color: STATUS_COLOR[to] ?? STATUS_COLOR.NO_DATA });
  }
  return out;
}

/** A backtest's lines as aligned columns, with a label per group. */
export function backtestData(bt: Backtest): { data: AlignedData; labels: string[] } {
  if (bt.series.length === 0) return { data: [[]], labels: [] };
  // Every line has one point per evaluation at the same ticks (api.md), so the
  // first line's x column is everyone's. Checked rather than assumed: a line
  // that disagrees is aligned by timestamp instead.
  const xs = bt.series[0]!.points.map((p) => p[0]);
  const same = bt.series.every((s) => s.points.length === xs.length && s.points.every((p, i) => p[0] === xs[i]));
  if (same) {
    return { data: [xs, ...bt.series.map((s) => s.points.map((p) => p[1]))], labels: bt.series.map((s) => s.group) };
  }
  const all = [...new Set(bt.series.flatMap((s) => s.points.map((p) => p[0])))].sort((a, b) => a - b);
  const index = new Map(all.map((x, i) => [x, i]));
  const ys = bt.series.map((s) => {
    const col: (number | null)[] = all.map(() => null);
    for (const [x, y] of s.points) col[index.get(x)!] = y;
    return col;
  });
  return { data: [all, ...ys], labels: bt.series.map((s) => s.group) };
}

/** The evaluation window as a shaded span. */
export function windowSpan(window: { from: number; to: number }): NonNullable<ChartAnnotations["spans"]>[number] {
  return { from: window.from, to: window.to, color: "#7c3aed22" };
}
