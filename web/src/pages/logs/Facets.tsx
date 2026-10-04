import type { Facets as FacetsData } from "../../lib/logsApi";

/** The keys the sidebar facets on, in order. */
export const FACET_KEYS = ["status", "service", "host", "source", "env"] as const;

/** Props for Facets. */
export interface FacetsProps {
  data: FacetsData | undefined;
  /** Called to add `key:value` (or its negation) to the query. */
  onPick: (key: string, value: string, negate: boolean) => void;
}

/**
 * The most frequent values of each key among the matching logs. Clicking a
 * value adds it to the query, shift-click excludes it: most queries are built
 * by narrowing rather than typed from scratch.
 */
export function Facets({ data, onPick }: FacetsProps) {
  if (!data) return <p className="text-sm text-zinc-500">Loading facets…</p>;
  return (
    <div className="space-y-4">
      {FACET_KEYS.map((key) => {
        const values = data.facets[key] ?? [];
        if (values.length === 0) return null;
        const top = Math.max(...values.map((v) => v.count));
        return (
          <section key={key} aria-label={`${key} facet`}>
            <h3 className="mb-1 text-xs font-semibold uppercase tracking-wide text-zinc-500">{key}</h3>
            <ul className="space-y-0.5">
              {values.map((v) => (
                <li key={v.value}>
                  <button
                    type="button"
                    title="Click to filter, shift-click to exclude"
                    onClick={(e) => onPick(key, v.value, e.shiftKey)}
                    className="relative flex w-full items-center justify-between rounded px-1.5 py-0.5 text-left text-sm hover:bg-zinc-100 dark:hover:bg-zinc-800"
                  >
                    <span
                      aria-hidden
                      className="absolute inset-y-0 left-0 rounded bg-sky-500/10"
                      style={{ width: `${(v.count / top) * 100}%` }}
                    />
                    <span className="relative truncate">{v.value || "(none)"}</span>
                    <span className="relative ml-2 tabular-nums text-zinc-500">{v.count}</span>
                  </button>
                </li>
              ))}
            </ul>
          </section>
        );
      })}
      {data.truncated && <p className="text-xs text-amber-600">Counts cover only part of the range (scan budget).</p>}
    </div>
  );
}
