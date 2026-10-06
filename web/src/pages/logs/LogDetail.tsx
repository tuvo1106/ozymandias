import { Link } from "react-router";
import { traceLink } from "../../lib/apmState";
import type { LogEntry } from "../../lib/logsApi";
import { flattenAttrs } from "../../lib/logsView";

/** Props for LogDetail. */
export interface LogDetailProps {
  log: LogEntry;
  onClose: () => void;
  /** Add a term to the query (filter, or exclude when `negate`). */
  onPick: (key: string, value: string, negate: boolean) => void;
  /** Toggle an attribute column. */
  onToggleColumn: (path: string) => void;
  columns: readonly string[];
}

const LABEL_FIELDS = ["service", "status", "host", "source"] as const;

/**
 * Everything about one log: its message in full (tracebacks keep their
 * lines), labels and attributes, each with filter / exclude / column
 * actions. The trace link is inert until tracing exists (M5); showing it now
 * keeps the finished shape visible, as the nav does.
 */
export function LogDetail({ log, onClose, onPick, onToggleColumn, columns }: LogDetailProps) {
  const attrs = flattenAttrs(log.attrs);
  const btn = "rounded px-1 text-xs text-zinc-500 hover:bg-zinc-200 dark:hover:bg-zinc-700";
  return (
    <aside aria-label="Log detail" className="flex h-full flex-col overflow-y-auto border-l border-zinc-200 p-4 dark:border-zinc-800">
      <div className="mb-3 flex items-center justify-between">
        <h2 className="font-semibold">{new Date(log.ts).toISOString()}</h2>
        <button type="button" onClick={onClose} aria-label="Close detail" className={btn}>
          ✕
        </button>
      </div>
      <pre className="mb-4 max-h-64 overflow-auto whitespace-pre-wrap break-words rounded bg-zinc-100 p-2 text-sm dark:bg-zinc-900">{log.message}</pre>
      <dl className="space-y-1 text-sm">
        {LABEL_FIELDS.map((k) =>
          log[k] ? (
            <Row key={k} name={k} value={String(log[k])} onPick={onPick} btn={btn} />
          ) : null,
        )}
        {(log.tags ?? []).map((t) => (
          <Row key={t} name="tag" value={t} btn={btn} />
        ))}
        {log.trace_id && (
          <div className="flex gap-2">
            <dt className="w-24 shrink-0 text-zinc-500">trace_id</dt>
            <dd className="break-all">
              <Link to={traceLink(log.trace_id)} className="text-violet-700 hover:underline dark:text-violet-300">
                {log.trace_id}
              </Link>
            </dd>
          </div>
        )}
        {attrs.map(([path, v]) => (
          <Row
            key={path}
            name={path}
            value={typeof v === "string" ? v : JSON.stringify(v)}
            onPick={(_, value, negate) => onPick(path, value, negate)}
            btn={btn}
            column={{ on: columns.includes(path), toggle: () => onToggleColumn(path) }}
          />
        ))}
      </dl>
    </aside>
  );
}

function Row({
  name,
  value,
  onPick,
  btn,
  column,
}: {
  name: string;
  value: string;
  onPick?: (key: string, value: string, negate: boolean) => void;
  btn: string;
  column?: { on: boolean; toggle: () => void };
}) {
  return (
    <div className="group flex gap-2">
      <dt className="w-24 shrink-0 truncate text-zinc-500" title={name}>
        {name}
      </dt>
      <dd className="min-w-0 flex-1 break-all">{value}</dd>
      <span className="invisible flex shrink-0 gap-1 group-hover:visible group-focus-within:visible">
        {onPick && (
          <>
            <button type="button" className={btn} aria-label={`Filter ${name}:${value}`} onClick={() => onPick(name, value, false)}>
              +
            </button>
            <button type="button" className={btn} aria-label={`Exclude ${name}:${value}`} onClick={() => onPick(name, value, true)}>
              −
            </button>
          </>
        )}
        {column && (
          <button type="button" className={btn} aria-pressed={column.on} aria-label={`Column ${name}`} onClick={column.toggle}>
            ▦
          </button>
        )}
      </span>
    </div>
  );
}
