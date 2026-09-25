import { useTagKeys } from "../../lib/useMetricsApi";

/** Props for GroupBySelect. */
export interface GroupBySelectProps {
  metric: string;
  by: string[];
  onChange: (by: string[]) => void;
}

/**
 * Multi-select of tag keys to group by, as a disclosure of checkboxes. Keys
 * already in the URL stay listed even if the server no longer reports them,
 * so a deep link can always be edited back. Order of `by` is the order of
 * selection, which is the order the API receives.
 */
export function GroupBySelect({ metric, by, onChange }: GroupBySelectProps) {
  const keys = useTagKeys(metric);
  const all = [...new Set([...by, ...(keys.data ?? [])])].sort();
  return (
    <details className="relative">
      <summary className="cursor-pointer list-none rounded-md border border-zinc-300 px-2 py-1 text-sm dark:border-zinc-700">
        by {by.length ? by.join(", ") : "everything"}
      </summary>
      <fieldset className="absolute z-10 mt-1 max-h-64 min-w-48 overflow-auto rounded-md border border-zinc-200 bg-white p-2 text-sm shadow-lg dark:border-zinc-700 dark:bg-zinc-900">
        <legend className="sr-only">Group by</legend>
        {!metric && <p className="text-zinc-500">Choose a metric first.</p>}
        {metric && keys.isPending && <p className="text-zinc-500">Loading…</p>}
        {metric && keys.isError && <p className="text-red-600 dark:text-red-400">{keys.error.message}</p>}
        {metric && keys.isSuccess && all.length === 0 && <p className="text-zinc-500">This metric has no tags.</p>}
        {all.map((k) => (
          <label key={k} className="flex items-center gap-2 py-0.5">
            <input
              type="checkbox"
              checked={by.includes(k)}
              onChange={(e) => onChange(e.target.checked ? [...by, k] : by.filter((x) => x !== k))}
            />
            {k}
          </label>
        ))}
      </fieldset>
    </details>
  );
}
