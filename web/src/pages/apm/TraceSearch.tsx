import { useInfiniteQuery } from "@tanstack/react-query";
import { useCallback, useMemo } from "react";
import { fetchTraces } from "../../lib/apmApi";
import { apmWindow, toTraceFilter } from "../../lib/apmState";
import { rangeKey } from "../../lib/useMetricsApi";
import { ApmHeader, BTN, ErrorNote, useApmState } from "./ApmShell";
import { DurationScatter } from "./DurationScatter";
import { TraceList } from "./TraceList";

const PAGE = 100;

const FIELD = "rounded-md border border-zinc-300 bg-transparent px-2 py-1 text-sm dark:border-zinc-700";

/** One text/number filter that applies on Enter or blur, so typing does not fire a search per key. */
function Filter({ label, value, onCommit, width = "w-32", placeholder }: { label: string; value: string; onCommit: (v: string) => void; width?: string; placeholder?: string }) {
  return (
    <label className="flex flex-col gap-0.5 text-xs text-zinc-500">
      {label}
      <input
        key={value}
        defaultValue={value}
        placeholder={placeholder}
        className={`${FIELD} ${width} text-zinc-900 dark:text-zinc-100`}
        onKeyDown={(e) => e.key === "Enter" && onCommit(e.currentTarget.value.trim())}
        onBlur={(e) => e.currentTarget.value.trim() !== value && onCommit(e.currentTarget.value.trim())}
      />
    </label>
  );
}

const ms = (v: string): number | undefined => (v !== "" && Number.isFinite(Number(v)) && Number(v) >= 0 ? Number(v) : undefined);

/**
 * Trace search: filters in the URL, a duration scatter over the searched
 * window, and the matching entry spans as a virtual list that pages by cursor
 * as it is scrolled. The search is over the *kept* traces: a selective filter
 * over a long window may stop early with a cursor ("examined" says how much
 * was scanned), and "Search further" continues from it.
 */
export function TraceSearch() {
  const { state, update } = useApmState();
  const rk = rangeKey(state.range);
  const filter = useMemo(() => toTraceFilter(state), [state]);
  const q = useInfiniteQuery({
    queryKey: ["apm", "traces", JSON.stringify(filter), rk],
    initialPageParam: undefined as string | undefined,
    queryFn: ({ pageParam, signal }) => fetchTraces(filter, apmWindow(state.range, Date.now()), { limit: PAGE, cursor: pageParam }, fetch, signal),
    getNextPageParam: (last) => last.cursor,
    retry: false,
  });
  const traces = useMemo(() => q.data?.pages.flatMap((p) => p.traces) ?? [], [q.data]);
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = q;
  const onNearEnd = useCallback(() => {
    if (hasNextPage && !isFetchingNextPage) void fetchNextPage();
  }, [hasNextPage, isFetchingNextPage, fetchNextPage]);
  // The window the scatter's x axis spans: resolved once per data load, not per render.
  const win = useMemo(() => apmWindow(state.range, q.dataUpdatedAt), [state.range, q.dataUpdatedAt]);
  const examined = q.data?.pages.reduce((a, p) => a + p.examined, 0) ?? 0;

  return (
    <div className="flex flex-col gap-3">
      <ApmHeader title="Traces" state={state} update={update}>
        <button type="button" className={BTN} onClick={() => void q.refetch()}>
          Refresh
        </button>
      </ApmHeader>
      <div className="flex flex-wrap items-end gap-3">
        <Filter label="Service" value={state.service} onCommit={(service) => update({ service })} />
        <Filter label="Resource" value={state.resource} width="w-48" onCommit={(resource) => update({ resource })} />
        <Filter label="Min ms" value={state.minMs === undefined ? "" : String(state.minMs)} width="w-20" onCommit={(v) => update({ minMs: ms(v) })} />
        <Filter label="Max ms" value={state.maxMs === undefined ? "" : String(state.maxMs)} width="w-20" onCommit={(v) => update({ maxMs: ms(v) })} />
        <label className="flex items-center gap-1 pb-1 text-sm">
          <input type="checkbox" checked={state.error} onChange={(e) => update({ error: e.target.checked })} />
          Errors only
        </label>
      </div>
      {q.error && <ErrorNote error={q.error} />}
      {q.isPending && !q.error ? (
        <p className="p-6 text-center text-zinc-500">Searching…</p>
      ) : q.data ? (
        <>
          <DurationScatter traces={traces} window={win} />
          <TraceList traces={traces} onNearEnd={onNearEnd} empty="No traces match. Only traces the sampler kept are searchable." />
          <p className="text-xs text-zinc-500">
            {traces.length} traces{hasNextPage ? "+" : ""} · {examined.toLocaleString("en-US")} index entries examined
            {hasNextPage && (
              <button type="button" className="ml-2 underline" disabled={isFetchingNextPage} onClick={() => void fetchNextPage()}>
                Search further
              </button>
            )}
          </p>
        </>
      ) : null}
    </div>
  );
}
