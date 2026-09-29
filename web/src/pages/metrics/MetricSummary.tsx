import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router";
import {
  fetchMetricCardinality,
  fetchMetricTags,
  type MetricCardinalityList,
  type MetricTags,
} from "../../lib/cardinalityApi";
import { useDebouncedValue } from "../../lib/useDebouncedValue";
import { MetricsTabs } from "./MetricsTabs";

/** How many metrics the list shows: the highest, which are the ones to look at. */
export const SUMMARY_LIMIT = 200;

/** How long the prefix box must be still before the list is asked again. */
export const PREFIX_DEBOUNCE_MS = 300;

/**
 * The Metric Summary (M3 §2): every metric with its series count, highest
 * first, and for the one chosen, the tag keys that make its series — the key
 * with the most values is almost always the reason a metric is high.
 *
 * The URL holds the filter and the choice (`?prefix=`, `?metric=`), so a
 * link to "this metric is too big" shows it. Counts come from the index and
 * have no time range: a series counts until retention drops it (ADR-0023).
 */
export function MetricSummary() {
  const [params, setParams] = useSearchParams();
  const prefix = params.get("prefix") ?? "";
  const metric = params.get("metric") ?? "";
  const set = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) {
      if (v) next.set(k, v);
      else next.delete(k);
    }
    setParams(next, { replace: true });
  };

  // The box is the author's; the URL follows it once typing pauses.
  const [text, setText] = useState(prefix);
  const [synced, setSynced] = useState(prefix);
  if (synced !== prefix) {
    setSynced(prefix);
    // The URL holds the trimmed box; when that is all that changed, the
    // space the author just typed stays where they typed it.
    if (text.trim() !== prefix) setText(prefix);
  }
  const settled = useDebouncedValue(text, PREFIX_DEBOUNCE_MS);
  // Only a settled box writes: after back/forward the box already holds the
  // URL's prefix while `settled` still holds the old one, and writing that
  // back would undo the navigation.
  const write = settled === text && settled.trim() !== prefix ? settled.trim() : undefined;
  useEffect(() => {
    if (write === undefined) return;
    setParams(
      (p) => {
        const next = new URLSearchParams(p);
        if (write) next.set("prefix", write);
        else next.delete("prefix");
        return next;
      },
      { replace: true },
    );
  }, [write, setParams]);

  const list = useQuery({
    queryKey: ["metrics", "cardinality", prefix],
    queryFn: async ({ signal }) => ({ asked: prefix, list: await fetchMetricCardinality(prefix, SUMMARY_LIMIT, fetch, signal) }),
    placeholderData: keepPreviousData,
    retry: false,
  });

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold">Metric Summary</h1>
      <MetricsTabs />
      <p className="max-w-3xl text-sm text-zinc-500">
        Every metric by how many series it has — each distinct set of tags is one series, and each costs storage and
        query time. Counts are of what the store holds, until retention drops it, not of the last hour.
      </p>
      <div className="grid gap-4 lg:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]">
        <section aria-label="Metrics" className="flex flex-col gap-2 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
          <label className="flex items-center gap-2 text-sm">
            <span className="text-zinc-500">Metrics starting with</span>
            <input
              type="search"
              value={text}
              onChange={(e) => setText(e.target.value)}
              placeholder="http."
              className="flex-1 rounded-md border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
            />
          </label>
          <MetricList prefix={prefix} query={list} selected={metric} onSelect={(m) => set({ metric: m })} />
        </section>
        <section aria-label="Tag keys" className="flex flex-col gap-2 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
          <TagPanel metric={metric} />
        </section>
      </div>
    </div>
  );
}

interface MetricListProps {
  prefix: string;
  query: ReturnType<typeof useQuery<{ asked: string; list: MetricCardinalityList }>>;
  selected: string;
  onSelect: (metric: string) => void;
}

/**
 * The list, or which kind of nothing it is: counting, failed, an empty
 * store, nothing under this prefix — or the previous prefix's list, dimmed
 * and named, while this one is counted.
 */
function MetricList({ prefix, query, selected, onSelect }: MetricListProps) {
  const answer = query.data;
  // No list beside an error: TanStack drops the previous key's placeholder
  // once the new key fails, so there is nothing stale to show here — the
  // "shows the failure, not the previous list" test pins that.
  if (query.error && !answer)
    return (
      <p role="alert" className="text-sm text-red-700 dark:text-red-400">
        Could not count the series: {query.error.message}
      </p>
    );
  if (!answer) return <p role="status" className="text-sm text-zinc-500">Counting series…</p>;
  const stale = answer.asked !== prefix;
  const { list } = answer;
  return (
    <>
      {stale ? (
        <p role="status" className="text-xs text-zinc-500">
          Counting for “{prefix}”… the list below is for {answer.asked ? `“${answer.asked}”` : "every metric"}.
        </p>
      ) : null}
      {list.total === 0 ? (
        <p className="text-sm text-zinc-500">
          {answer.asked ? `No metric starts with “${answer.asked}”.` : "This ozyd holds no metrics yet."}
        </p>
      ) : (
        <div className={stale ? "opacity-40" : undefined}>
          {list.truncated ? (
            <p className="mb-1 text-xs text-zinc-500">
              The {list.metrics.length} highest of {list.total}. Narrow the prefix to see the rest.
            </p>
          ) : null}
          <table aria-label="Series per metric" className="w-full text-left text-sm tabular-nums">
            <thead className="text-xs text-zinc-500">
              <tr>
                <th className="py-1 font-medium">Metric</th>
                <th className="py-1 font-medium">Type</th>
                <th className="py-1 text-right font-medium">Series</th>
              </tr>
            </thead>
            <tbody>
              {list.metrics.map((m) => (
                <tr
                  key={m.name}
                  aria-selected={m.name === selected}
                  className="border-t border-zinc-100 aria-selected:bg-violet-50 dark:border-zinc-800 dark:aria-selected:bg-violet-950"
                >
                  <td className="py-1">
                    <button type="button" onClick={() => onSelect(m.name)} className="font-mono text-xs hover:underline">
                      {m.name}
                    </button>
                  </td>
                  <td className="py-1 text-xs text-zinc-500">{m.type ?? "not recorded"}</td>
                  <td className="py-1 text-right">{m.series.toLocaleString()}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/**
 * The chosen metric's tag keys, or which kind of nothing: none chosen,
 * counting, failed, a metric this store does not have, or one whose series
 * carry no tags — two answers the keys alone cannot tell apart, which is why
 * the answer carries the metric's own series count.
 */
function TagPanel({ metric }: { metric: string }) {
  const query = useQuery<MetricTags>({
    queryKey: ["metrics", "tagCardinality", metric],
    queryFn: ({ signal }) => fetchMetricTags(metric, fetch, signal),
    enabled: metric !== "",
    retry: false,
  });
  if (!metric) return <p className="text-sm text-zinc-500">Choose a metric to see which tag keys make its series.</p>;
  const heading = <h2 className="font-mono text-sm font-semibold">{metric}</h2>;
  if (query.error)
    return (
      <>
        {heading}
        <p role="alert" className="text-sm text-red-700 dark:text-red-400">
          Could not count its tag keys: {query.error.message}
        </p>
      </>
    );
  if (!query.data)
    return (
      <>
        {heading}
        <p role="status" className="text-sm text-zinc-500">
          Counting its tag keys…
        </p>
      </>
    );
  const t = query.data;
  if (t.series === 0)
    return (
      <>
        {heading}
        <p className="text-sm text-zinc-500">This ozyd holds no series of {metric}.</p>
      </>
    );
  const explore = `/metrics/explorer?${new URLSearchParams({ q: `avg:${metric}{*}` })}`;
  return (
    <>
      <div className="flex items-baseline justify-between gap-2">
        {heading}
        <Link to={explore} className="text-xs underline">
          Chart it
        </Link>
      </div>
      <p className="text-xs text-zinc-500">
        {t.series.toLocaleString()} series · {t.type ?? "type not recorded"}
      </p>
      {t.keys.length === 0 ? (
        <p className="text-sm text-zinc-500">Its series carry no tags: it is one series per metric name, which is as low as it goes.</p>
      ) : (
        <table aria-label="Series per tag key" className="w-full text-left text-sm tabular-nums">
          <thead className="text-xs text-zinc-500">
            <tr>
              <th className="py-1 font-medium">Tag key</th>
              <th className="py-1 text-right font-medium">Values</th>
              <th className="py-1 text-right font-medium">On series</th>
            </tr>
          </thead>
          <tbody>
            {t.keys.map((k) => (
              <tr key={k.key} className="border-t border-zinc-100 dark:border-zinc-800">
                <td className="py-1 font-mono text-xs">{k.key}</td>
                <td className="py-1 text-right">{k.values.toLocaleString()}</td>
                <td className="py-1 text-right text-zinc-500">
                  {k.series === t.series ? "all" : `${k.series.toLocaleString()} of ${t.series.toLocaleString()}`}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}
