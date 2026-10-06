import { useNavigate } from "react-router";
import type { TraceSummary } from "../../lib/apmApi";
import { traceLink } from "../../lib/apmState";
import { scatterLayout } from "../../lib/apmView";
import { formatDuration } from "../../lib/flame";

/** Props for DurationScatter. */
export interface DurationScatterProps {
  traces: readonly TraceSummary[];
  /** The searched window, unix ms: the x axis is this, not the span of the data. */
  window: { from: number; to: number };
}

const W = 900;
const H = 140;
const PAD = { l: 46, r: 8, t: 6, b: 18 };
/** Dots drawn at most; the list below is the complete answer. */
export const MAX_DOTS = 2000;

/**
 * Traces as dots: time across, duration up (log scale), failures in red. A
 * cluster of slow dots or a red streak is visible before any row is read, and
 * a dot opens its trace. The x axis is the searched window, as in the log
 * histogram: a burst of ten seconds in "the last day" is a thin streak at the
 * edge, not a block as wide as the page.
 */
export function DurationScatter({ traces, window: win }: DurationScatterProps) {
  const navigate = useNavigate();
  if (traces.length === 0) {
    return <div className="flex h-24 items-center justify-center text-sm text-zinc-500">No traces to plot</div>;
  }
  const shown = traces.length > MAX_DOTS ? traces.slice(0, MAX_DOTS) : traces;
  const s = scatterLayout(shown, win);
  const iw = W - PAD.l - PAD.r;
  const ih = H - PAD.t - PAD.b;
  return (
    <svg role="img" aria-label={`Scatter of ${shown.length} traces by time and duration`} viewBox={`0 0 ${W} ${H}`} className="w-full">
      <text x={PAD.l - 4} y={PAD.t + 8} textAnchor="end" className="fill-zinc-500 text-[10px]">
        {formatDuration(s.maxUs)}
      </text>
      <text x={PAD.l - 4} y={PAD.t + ih} textAnchor="end" className="fill-zinc-500 text-[10px]">
        {formatDuration(s.minUs)}
      </text>
      <text x={PAD.l} y={H - 4} className="fill-zinc-500 text-[10px]">
        {new Date(win.from).toLocaleTimeString()}
      </text>
      <text x={W - PAD.r} y={H - 4} textAnchor="end" className="fill-zinc-500 text-[10px]">
        {new Date(win.to).toLocaleTimeString()}
      </text>
      <line x1={PAD.l} x2={W - PAD.r} y1={PAD.t + ih} y2={PAD.t + ih} className="stroke-zinc-300 dark:stroke-zinc-700" />
      {s.points.map((p, i) => (
        <circle
          key={`${p.traceId}/${i}`}
          role="link"
          tabIndex={0}
          aria-label={`${p.error ? "failed " : ""}trace, ${formatDuration(p.durationUs)}`}
          cx={PAD.l + p.x * iw}
          cy={PAD.t + 4 + p.y * (ih - 8)}
          r={3}
          className={`cursor-pointer ${p.error ? "fill-red-500" : "fill-violet-500"} opacity-70 hover:opacity-100`}
          onClick={() => navigate(traceLink(p.traceId))}
          onKeyDown={(e) => e.key === "Enter" && navigate(traceLink(p.traceId))}
        >
          <title>{`${formatDuration(p.durationUs)}${p.error ? " — failed" : ""}`}</title>
        </circle>
      ))}
    </svg>
  );
}
