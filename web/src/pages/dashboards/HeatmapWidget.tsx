/**
 * The heatmap: a distribution's shape over time, drawn from the sketch behind
 * a metric rather than from a number taken out of it.
 *
 * It is the one widget not drawn from the batch. `/api/v1/query/batch` answers
 * in series of floats and a distribution is not one, so it has its own
 * endpoint and its own shape (ADR-0019) — which means its failures arrive
 * differently too: a refused query there is a refused *request*, so the error
 * comes from the fetch rather than from a per-query status. The frame draws it
 * the same either way.
 *
 * What the widget adds on top of the canvas is everything the picture cannot
 * say for itself: how accurate the bands are, how many groups it is not
 * showing, and how many observations the axis has no room for.
 */
import { useMemo } from "react";
import { HeatmapChart } from "../../charts/HeatmapChart";
import {
  axisRange,
  offAxisCount,
  primarySeries,
  relativeAccuracy,
  unboundedCount,
  wantsLogAxis,
} from "../../lib/heatmap";
import { WidgetFrame } from "./WidgetFrame";
import type { WidgetProps } from "./widgets";

/** A count as a sentence fragment, pluralised. */
function plural(n: number, one: string, many: string): string {
  return `${n.toLocaleString()} ${n === 1 ? one : many}`;
}

/** How to name the group being drawn, when a `by` produced several. */
function groupLabel(tags: Record<string, string> | undefined): string {
  const pairs = Object.entries(tags ?? {}).map(([k, v]) => `${k}:${v}`);
  return pairs.length ? pairs.join(", ") : "the first group";
}

/** Draws a distribution over time. */
export function HeatmapWidget({
  widget,
  sketch,
  xRange,
  syncKey,
  hiddenWarnings,
}: WidgetProps) {
  const { series, others } = useMemo(
    () => primarySeries(sketch?.data?.series ?? []),
    [sketch?.data?.series],
  );
  const buckets = series?.buckets ?? [];
  const log = wantsLogAxis(widget.yaxis);
  const range = axisRange(buckets, widget.yaxis, log);

  // Every note the picture cannot make: an error bar, the groups this widget
  // is not the place for, and the observations the axis cannot hold.
  const notes = (sketch?.data?.warnings ?? []).filter(
    (w) => !hiddenWarnings?.has(w),
  );
  const alpha = relativeAccuracy(buckets);
  if (alpha > 0)
    notes.push(`Bands are accurate to ±${(alpha * 100).toFixed(1)}%`);
  if (others > 0) {
    notes.push(
      `Showing ${groupLabel(series?.tags)} only — ${plural(others, "other group", "other groups")} matched. Two distributions drawn over each other are unreadable; use one widget each.`,
    );
  }
  const offAxis = offAxisCount(buckets, log);
  if (offAxis > 0)
    notes.push(
      `${plural(offAxis, "observation", "observations")} at or below zero, which a log axis cannot show`,
    );
  const unbounded = unboundedCount(buckets);
  if (unbounded > 0) {
    notes.push(
      `${plural(unbounded, "observation is", "observations are")} in a bin whose bounds are past what a number can hold, so they are not drawn`,
    );
  }

  // "No data" only once an answer has arrived. Before that the frame is blank:
  // a widget that says "No data" while it is still asking has told the reader
  // their service is down, which is the one thing a dashboard must not do.
  const answered = sketch?.data !== undefined;
  const drawable =
    answered &&
    range !== undefined &&
    buckets.length > 0 &&
    xRange !== undefined;

  return (
    <WidgetFrame
      title={widget.title}
      error={sketch?.error}
      warnings={notes}
      empty={answered && !drawable && !sketch?.error}
    >
      {drawable && range && xRange && sketch?.data ? (
        <HeatmapChart
          buckets={buckets}
          interval={sketch.data.interval}
          xRange={xRange}
          yRange={[range.lo, range.hi]}
          log={log}
          syncKey={syncKey}
        />
      ) : null}
    </WidgetFrame>
  );
}
