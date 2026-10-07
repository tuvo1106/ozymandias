import { useEffect, useState } from "react";
import { Link } from "react-router";
import type { TraceSummary } from "../../lib/apmApi";
import { traceLink } from "../../lib/apmState";
import { fmtMs } from "../../lib/apmView";
import { visibleRange } from "../../lib/logsView";
import { formatTs } from "../logs/LogList";

/** Fixed row height: what lets the list be virtual without measuring. */
export const TRACE_ROW_HEIGHT = 28;

/** Props for TraceList. */
export interface TraceListProps {
  traces: readonly TraceSummary[];
  /** Called when the user scrolls near the end: fetch the next page. */
  onNearEnd: () => void;
  height?: number;
  /** Shown instead of rows when there are none. */
  empty?: string;
}

/**
 * Entry spans as a virtual list: only the rows in view are in the DOM, so a
 * long search costs what a short one does. A row is a link to the trace view,
 * which makes it middle-clickable and keyboard reachable without row-level
 * handlers. Failed entry spans, and traces where some other span failed, are
 * told apart: the second is not the user's error, but is the one to look at.
 */
export function TraceList({ traces, onNearEnd, height = 360, empty = "No traces match." }: TraceListProps) {
  const [scrollTop, setScrollTop] = useState(0);
  const { start, end } = visibleRange(scrollTop, height, TRACE_ROW_HEIGHT, traces.length);
  const nearEnd = traces.length > 0 && end >= traces.length - 10;
  useEffect(() => {
    if (nearEnd) onNearEnd();
  }, [nearEnd, traces.length, onNearEnd]);

  if (traces.length === 0) return <p className="p-6 text-center text-zinc-500">{empty}</p>;
  return (
    <div role="list" aria-label="Traces" className="overflow-y-auto overflow-x-hidden text-sm" style={{ height }} onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}>
      <div style={{ height: traces.length * TRACE_ROW_HEIGHT, position: "relative" }}>
        {traces.slice(start, end).map((t, i) => (
          <div
            key={`${t.trace_id}/${t.span_id}/${t.start}`}
            role="listitem"
            className="absolute inset-x-0 border-b border-zinc-100 hover:bg-zinc-50 dark:border-zinc-900 dark:hover:bg-zinc-900"
            style={{ top: (start + i) * TRACE_ROW_HEIGHT, height: TRACE_ROW_HEIGHT }}
          >
            <Link to={traceLink(t.trace_id)} className="flex h-full items-center gap-3 whitespace-nowrap px-2">
              <span
                aria-hidden
                className={`h-4 w-1 shrink-0 rounded ${t.error ? "bg-red-500" : t.trace_error ? "bg-amber-500" : "bg-emerald-500"}`}
              />
              <span className="sr-only">{t.error ? "failed" : t.trace_error ? "a span in this trace failed" : "ok"}</span>
              <span className="w-32 shrink-0 font-mono text-xs text-zinc-500">{formatTs(t.start / 1000)}</span>
              <span className="w-28 shrink-0 truncate">{t.service}</span>
              <span className="min-w-0 flex-1 truncate" title={`${t.name} ${t.resource}`}>
                {t.resource || t.name}
              </span>
              {t.status_code ? <span className="w-10 shrink-0 text-xs text-zinc-500">{t.status_code}</span> : null}
              <span className="w-20 shrink-0 text-right tabular-nums">{fmtMs(t.duration / 1000)}</span>
            </Link>
          </div>
        ))}
      </div>
    </div>
  );
}
