import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useCallback, useMemo, useState } from "react";
import { useSearchParams } from "react-router";
import { fetchFacets, fetchHistogram, fetchLogs, type LogEntry, type LogsQuery } from "../../lib/logsApi";
import { parseLogsState, serializeLogsState, type LogsState } from "../../lib/logsState";
import { addTerm, completions, deleteView, loadViews, mergeTail, rowKey, saveView, type SavedView } from "../../lib/logsView";
import { TAIL_CAP, useLogTail } from "../../lib/useLogTail";
import { rangeKey } from "../../lib/useMetricsApi";
import { resolveTimeRange } from "../../lib/timeRange";
import { Autocomplete } from "../../ui/Autocomplete";
import { TimeRangePicker } from "../metrics/TimeRangePicker";
import { FACET_KEYS, Facets } from "./Facets";
import { Histogram } from "./Histogram";
import { LogDetail } from "./LogDetail";
import { LogList } from "./LogList";

const PAGE = 200;

/** The window a request covers, in unix ms, resolved against the clock now. */
function windowOf(state: LogsState, nowMs: number): LogsQuery {
  const r = resolveTimeRange(state.range, nowMs);
  return { q: state.q, from: r.from * 1000, to: r.to * 1000 + 999 };
}

/**
 * The Log Explorer: a query bar over a histogram, with facets on the left, a
 * virtual list in the middle and a detail panel on the right. The URL holds
 * the question (query, range, tail, columns), so any view is a link.
 *
 * Tailing reads the recent past from the search API first and then streams:
 * the stream carries only what arrives afterwards, so the two are stitched
 * (and de-duplicated, since a log can be in both) in the list.
 */
export function LogExplorer() {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseLogsState(params), [params]);
  const update = useCallback(
    (patch: Partial<LogsState>) => setParams(serializeLogsState({ ...state, ...patch }), { replace: false }),
    [state, setParams],
  );
  const [draft, setDraft] = useState(state.q);
  // The URL can change under the box (a facet click, a saved view, back): the draft follows it.
  const [seenQ, setSeenQ] = useState(state.q);
  if (seenQ !== state.q) {
    setSeenQ(state.q);
    setDraft(state.q);
  }

  const rk = rangeKey(state.range);
  const logsQ = useInfiniteQuery({
    queryKey: ["logs", "list", state.q, rk],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => fetchLogs(windowOf(state, Date.now()), { limit: PAGE, cursor: pageParam }, fetch, signal),
    getNextPageParam: (last) => last.cursor,
    retry: false,
  });
  const histQ = useQuery({
    queryKey: ["logs", "hist", state.q, rk],
    queryFn: ({ signal }) => fetchHistogram(windowOf(state, Date.now()), "status", fetch, signal),
    retry: false,
  });
  const facetQ = useQuery({
    queryKey: ["logs", "facets", state.q, rk],
    queryFn: ({ signal }) => fetchFacets(windowOf(state, Date.now()), FACET_KEYS, fetch, signal),
    retry: false,
  });

  const tail = useLogTail(state.q, state.tail);

  const pageLogs = useMemo(() => logsQ.data?.pages.flatMap((p) => p.logs) ?? [], [logsQ.data]);
  const logs = useMemo(() => (state.tail ? mergeTail(pageLogs, [...tail.logs].reverse(), TAIL_CAP + PAGE * 10) : pageLogs), [pageLogs, tail.logs, state.tail]);
  const truncated = logsQ.data?.pages.some((p) => p.truncated) ?? false;

  const [selected, setSelected] = useState<LogEntry | undefined>();
  const [views, setViews] = useState<SavedView[]>(() => loadViews());
  const [colDraft, setColDraft] = useState("");

  const run = (q: string) => update({ q: q.trim() });
  const pick = (key: string, value: string, negate: boolean) => run(addTerm(state.q, key, value, negate));
  const toggleCol = (path: string) =>
    update({ cols: state.cols.includes(path) ? state.cols.filter((c) => c !== path) : [...state.cols, path] });
  const fetchNext = logsQ.fetchNextPage;
  const hasNext = logsQ.hasNextPage;
  const fetching = logsQ.isFetchingNextPage;
  const onNearEnd = useCallback(() => {
    if (hasNext && !fetching && !state.tail) void fetchNext();
  }, [hasNext, fetching, fetchNext, state.tail]);

  const suggestions = useMemo(() => completions(draft, facetQ.data?.facets ?? {}), [draft, facetQ.data]);
  const error = logsQ.error ?? histQ.error;
  const btn = "rounded-md border border-zinc-300 px-2 py-1 text-sm hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800";

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <h1 className="text-xl font-semibold">Logs</h1>
        <Autocomplete
          label="Log query"
          className="min-w-[20rem] flex-1"
          value={draft}
          onChange={setDraft}
          options={suggestions}
          placeholder='service:web-api status:error "timeout" @route:/orders'
          onSubmit={(v) => (v === draft ? run(v) : setDraft(v))}
        />
        <button type="button" className={btn} onClick={() => run(draft)}>
          Search
        </button>
        <TimeRangePicker key={JSON.stringify(state.range)} range={state.range} onChange={(range) => update({ range })} />
        <button
          type="button"
          className={`${btn} ${state.tail ? "border-emerald-500 text-emerald-600" : ""}`}
          aria-pressed={state.tail}
          onClick={() => update({ tail: !state.tail })}
        >
          {state.tail ? (tail.connected ? "● Live" : "○ Connecting…") : "Live tail"}
        </button>
        {state.tail && (
          <button type="button" className={btn} aria-pressed={tail.paused} onClick={() => tail.setPaused(!tail.paused)}>
            {tail.paused ? `Resume (${tail.waiting} waiting)` : "Pause"}
          </button>
        )}
        {!state.tail && (
          <button type="button" className={btn} onClick={() => void Promise.all([logsQ.refetch(), histQ.refetch(), facetQ.refetch()])}>
            Refresh
          </button>
        )}
      </div>

      <div className="flex flex-wrap items-center gap-2 text-sm">
        <span className="text-zinc-500">Columns:</span>
        {state.cols.map((c) => (
          <button key={c} type="button" className={btn} onClick={() => toggleCol(c)} aria-label={`Remove column ${c}`}>
            {c} ✕
          </button>
        ))}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            const c = colDraft.trim().replace(/^@/, "");
            if (c && !state.cols.includes(c)) toggleCol(c);
            setColDraft("");
          }}
        >
          <input
            aria-label="Add column"
            value={colDraft}
            onChange={(e) => setColDraft(e.target.value)}
            placeholder="+ attribute"
            className="w-28 rounded-md border border-zinc-300 bg-transparent px-2 py-1 dark:border-zinc-700"
          />
        </form>
        <span className="ml-auto flex items-center gap-2">
          <select
            aria-label="Saved views"
            value=""
            onChange={(e) => {
              const v = views.find((x) => x.name === e.target.value);
              if (v) setParams(new URLSearchParams(v.search));
            }}
            className="rounded-md border border-zinc-300 bg-transparent px-2 py-1 dark:border-zinc-700"
          >
            <option value="">Saved views…</option>
            {views.map((v) => (
              <option key={v.name} value={v.name}>
                {v.name}
              </option>
            ))}
          </select>
          <button
            type="button"
            className={btn}
            onClick={() => {
              const name = window.prompt("Name this view")?.trim();
              if (name) setViews(saveView({ name, search: serializeLogsState(state).toString() }));
            }}
          >
            Save view
          </button>
          {views.length > 0 && (
            <button
              type="button"
              className={btn}
              aria-label="Delete a saved view"
              onClick={() => {
                const name = window.prompt(`Delete which view? ${views.map((v) => v.name).join(", ")}`)?.trim();
                if (name) setViews(deleteView(name));
              }}
            >
              Delete view
            </button>
          )}
        </span>
      </div>

      {error && (
        <p role="alert" className="rounded-md bg-red-50 p-3 text-sm text-red-700 dark:bg-red-950 dark:text-red-300">
          {error.message}
        </p>
      )}
      {tail.dropped > 0 && state.tail && (
        <p role="status" className="rounded-md bg-amber-50 p-2 text-sm text-amber-800 dark:bg-amber-950 dark:text-amber-200">
          The tail could not keep up: {tail.dropped} logs were skipped. Narrow the query to see them all.
        </p>
      )}

      <Histogram
        data={histQ.data}
        onSelect={(from, to) => update({ range: { kind: "absolute", from: Math.floor(from / 1000), to: Math.max(Math.ceil(to / 1000), Math.floor(from / 1000) + 1) }, tail: false })}
      />

      <div className="grid gap-3" style={{ gridTemplateColumns: selected ? "13rem 1fr 22rem" : "13rem 1fr" }}>
        <Facets data={facetQ.data} onPick={pick} />
        <div className="min-w-0">
          {logsQ.isPending && !error ? (
            <p className="p-6 text-center text-zinc-500">Searching…</p>
          ) : (
            <LogList
              logs={logs}
              columns={state.cols}
              selected={selected ? rowKey(selected) : undefined}
              onSelect={setSelected}
              onNearEnd={onNearEnd}
            />
          )}
          <p className="mt-1 text-xs text-zinc-500">
            {logs.length} logs{hasNext ? "+" : ""}
            {truncated && " — the scan budget ran out; this is the newest part of the range, not all of it"}
            {truncated && hasNext && !state.tail && (
              <button type="button" className="ml-2 underline" disabled={fetching} onClick={() => void fetchNext()}>
                Search further back
              </button>
            )}
            {logsQ.data?.pages[0]?.stats && ` · ${logsQ.data.pages[0].stats.blocks_read} blocks read, ${logsQ.data.pages[0].stats.blocks_skipped} skipped`}
          </p>
        </div>
        {selected && (
          <LogDetail log={selected} columns={state.cols} onClose={() => setSelected(undefined)} onPick={pick} onToggleColumn={toggleCol} />
        )}
      </div>
    </div>
  );
}

