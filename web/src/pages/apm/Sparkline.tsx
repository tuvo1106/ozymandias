import { sparkPoints } from "../../lib/apmView";

/** Props for Sparkline. */
export interface SparklineProps {
  values: readonly number[];
  width?: number;
  height?: number;
  /** Accessible description, e.g. "shop requests". */
  label: string;
}

/**
 * A tiny trend line with no axes: shape only, the table carries the numbers.
 * Fewer than two points is drawn as a dash, not a flat line at zero: a flat
 * line claims "traffic was steady", a dash says "nothing to show".
 */
export function Sparkline({ values, width = 96, height = 22, label }: SparklineProps) {
  const points = sparkPoints(values, width, height);
  if (!points) {
    return (
      <span role="img" aria-label={`${label}: no data`} className="text-zinc-400">
        —
      </span>
    );
  }
  return (
    <svg role="img" aria-label={label} width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="text-violet-500">
      <polyline points={points} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinejoin="round" strokeLinecap="round" />
    </svg>
  );
}
