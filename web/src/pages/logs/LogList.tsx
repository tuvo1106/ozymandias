import { useEffect, useRef, useState } from "react";
import type { LogEntry } from "../../lib/logsApi";
import { attrText, rowKey, statusColor, visibleRange } from "../../lib/logsView";

/** Fixed row height: what lets the list be virtual without measuring. */
export const ROW_HEIGHT = 24;

/** Props for LogList. */
export interface LogListProps {
  logs: readonly LogEntry[];
  columns: readonly string[];
  selected: string | undefined;
  onSelect: (log: LogEntry) => void;
  /** Called when the user scrolls near the end: fetch the next page. */
  onNearEnd: () => void;
  /** Height of the scrolling viewport, px. */
  height?: number;
}

/**
 * A virtualized list: only the rows in view (plus overscan) are in the DOM,
 * so tens of thousands of logs cost the same as fifty. Rows are single-line
 * and fixed-height; the full message is in the detail panel.
 */
export function LogList({ logs, columns, selected, onSelect, onNearEnd, height = 480 }: LogListProps) {
  const ref = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const { start, end } = visibleRange(scrollTop, height, ROW_HEIGHT, logs.length);
  const nearEnd = logs.length > 0 && end >= logs.length - 20;
  useEffect(() => {
    if (nearEnd) onNearEnd();
  }, [nearEnd, logs.length, onNearEnd]);

  if (logs.length === 0) return <p className="p-6 text-center text-zinc-500">No logs match.</p>;
  return (
    <div
      ref={ref}
      role="list"
      aria-label="Logs"
      tabIndex={0}
      className="overflow-y-auto overflow-x-hidden font-mono text-xs"
      style={{ height }}
      onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
    >
      <div style={{ height: logs.length * ROW_HEIGHT, position: "relative" }}>
        {logs.slice(start, end).map((l, i) => {
          const key = rowKey(l);
          return (
            <div
              key={key}
              role="listitem"
              aria-selected={selected === key}
              onClick={() => onSelect(l)}
              className={`absolute inset-x-0 flex cursor-pointer items-center gap-2 whitespace-nowrap px-2 hover:bg-zinc-100 dark:hover:bg-zinc-800 ${selected === key ? "bg-sky-50 dark:bg-sky-950" : ""}`}
              style={{ top: (start + i) * ROW_HEIGHT, height: ROW_HEIGHT }}
            >
              <span aria-hidden className={`h-4 w-1 shrink-0 rounded ${statusColor(l.status)}`} title={l.status} />
              <span className="w-[8.5rem] shrink-0 text-zinc-500">{formatTs(l.ts)}</span>
              <span className="w-24 shrink-0 truncate text-zinc-600 dark:text-zinc-400">{l.service}</span>
              {columns.map((c) => (
                <span key={c} className="w-24 shrink-0 truncate text-zinc-600 dark:text-zinc-400" title={`${c}: ${attrText(l, c)}`}>
                  {attrText(l, c)}
                </span>
              ))}
              <span className="min-w-0 flex-1 truncate">{l.message.split("\n")[0]}</span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/** `MM-DD HH:MM:SS.mmm` in local time: the date matters when a range spans midnight. */
export function formatTs(ms: number): string {
  const d = new Date(ms);
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}
