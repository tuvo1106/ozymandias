import type { Histogram as HistogramData } from "../../lib/logsApi";
import { STATUS_ORDER, statusColor } from "../../lib/logsView";

/** Props for Histogram. */
export interface HistogramProps {
  data: HistogramData | undefined;
  /** Called with a bar's window (unix ms) when it is clicked: narrow to it. */
  onSelect: (from: number, to: number) => void;
}

/**
 * Bars of matching logs over time, each stacked by status and coloured by
 * severity so a spike of errors is visible before a single row is read. The
 * server's buckets are sparse (only bars that hold a log), so the bars are
 * placed by timestamp, not by index. Clicking a bar narrows the range to it.
 */
export function Histogram({ data, onSelect }: HistogramProps) {
  if (!data || data.buckets.length === 0) {
    return <div className="flex h-20 items-center justify-center text-sm text-zinc-500">No logs in this range</div>;
  }
  // The axis is the window that was queried: 40 logs in four seconds of "the last hour"
  // are one thin bar at the right edge, not a block as wide as the page. Bars are aligned
  // to multiples of the interval, so the first slot starts at the window's start rounded down.
  const first = Math.floor(data.from / data.interval_ms) * data.interval_ms;
  const slots = Math.max(1, Math.ceil((data.to + 1 - first) / data.interval_ms));
  const total = (c: Record<string, number>) => Object.values(c).reduce((a, b) => a + b, 0);
  const max = Math.max(...data.buckets.map((b) => total(b.counts)));
  const rank = (s: string) => {
    const i = (STATUS_ORDER as readonly string[]).indexOf(s);
    return i < 0 ? -1 : i;
  };
  return (
    <div
      role="img"
      aria-label={`Histogram of ${data.buckets.length} time buckets`}
      className="relative flex h-20 items-end gap-px"
      style={{ width: "100%" }}
    >
      {data.buckets.map((b) => {
        const left = (Math.max(0, b.ts - first) / data.interval_ms / slots) * 100;
        const n = total(b.counts);
        return (
          <button
            key={b.ts}
            type="button"
            title={`${new Date(b.ts).toLocaleString()}: ${n} logs`}
            aria-label={`${new Date(b.ts).toISOString()} ${n} logs`}
            onClick={() => onSelect(b.ts, b.ts + data.interval_ms - 1)}
            className="absolute bottom-0 flex flex-col-reverse justify-start hover:opacity-80"
            style={{ left: `${left}%`, width: `${Math.max(100 / slots - 0.1, 0.5)}%`, height: `${(n / max) * 100}%` }}
          >
            {Object.entries(b.counts)
              .sort(([a], [c]) => rank(a) - rank(c))
              .map(([status, count]) => (
                <span key={status} className={statusColor(status)} style={{ height: `${(count / n) * 100}%` }} />
              ))}
          </button>
        );
      })}
    </div>
  );
}
