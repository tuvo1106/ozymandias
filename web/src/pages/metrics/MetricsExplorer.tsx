import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type ReactNode } from "react";
import { useSearchParams } from "react-router";
import { TimeseriesChart } from "../../charts/TimeseriesChart";
import { reduceSeries, type Reducer } from "../../lib/dashboard";
import { alignSeries, formatValue, seriesLabel } from "../../lib/chartData";
import { parseExplorerState, serializeExplorerState, type ExplorerState } from "../../lib/explorerState";
import { ApiError, type QueryResult } from "../../lib/metricsApi";
import {
  explorerKey,
  REFRESH_INTERVAL_MS,
  shouldAutoRefresh,
  useExplorerQuery,
  useLegacyTranslation,
  type ExplorerAnswer,
} from "../../lib/useMetricsApi";
import { QueryEditor } from "../dashboards/editor/QueryEditor";
import { MetricsTabs } from "./MetricsTabs";
import { SaveToDashboard } from "./SaveToDashboard";
import { TimeRangePicker } from "./TimeRangePicker";

/**
 * The Metrics Explorer: write a metricql query, run it, chart it, keep it.
 *
 * The URL holds the query that was *run* (lib/explorerState.ts); the box
 * holds a draft beside it, with the dashboard editor's completion and parse
 * errors, and Run — or Ctrl/⌘+Enter — is what charts the draft. So the chart
 * always answers a query the author finished, a link always names the query
 * its chart answers, and back/forward steps through queries rather than
 * keystrokes (ADR-0022).
 *
 * A link with M1's parameters instead of `q` is sent to the server as it is,
 * and the query the server says it ran replaces them in the URL — its answer
 * seeding the chart, so nothing is asked twice. If the server refuses them,
 * the page says so in its words, and the parameters stay in the URL.
 */
export function MetricsExplorer() {
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseExplorerState(params), [params]);
  const update = (patch: Partial<ExplorerState>) => setParams(serializeExplorerState({ ...state, ...patch }));
  const query = useExplorerQuery(state);
  const legacy = useLegacyTranslation(state);
  const client = useQueryClient();
  const autoRefresh = shouldAutoRefresh(state);

  // An M1 link, translated: from here on it is an ordinary `q` link. Replace,
  // not push — back should not return to parameters that mean the same.
  //
  // Its answer seeds the chart, unless the cache already holds one at least
  // as new: reopening a link whose translation is cached would otherwise
  // write that old answer over the chart's own, stamped as if just fetched.
  // `updatedAt` records the seed's true age too, though as the cache is
  // pruned today a seed is only ever written the moment it arrives.
  const translation = legacy.data?.query ? legacy.data : undefined;
  const translatedAt = legacy.dataUpdatedAt;
  useEffect(() => {
    if (state.q || !state.legacy || !translation) return;
    const key = explorerKey(translation.query, state.range);
    if ((client.getQueryState(key)?.dataUpdatedAt ?? 0) < translatedAt)
      client.setQueryData<ExplorerAnswer>(key, { asked: translation.query, result: translation }, { updatedAt: translatedAt });
    setParams(serializeExplorerState({ ...state, q: translation.query, legacy: "" }), { replace: true });
  }, [state, translation, translatedAt, client, setParams]);

  // The draft follows the URL when the URL changes under it — back/forward,
  // a pasted link — and otherwise is the author's. Adjusted during render,
  // not by remounting the box, which would drop the focus on every run.
  //
  // One change is not the URL moving: an M1 link's translation landing is
  // the page catching up with what was already asked, and the author may
  // have typed while it was on its way. What they typed stays theirs; the
  // box then reads as an edit of the translated query, and says so.
  const [draft, setDraft] = useState(state.q);
  const [synced, setSynced] = useState({ q: state.q, legacy: state.legacy });
  if (synced.q !== state.q || synced.legacy !== state.legacy) {
    const translationLanded = synced.q === "" && synced.legacy !== "" && state.q !== "" && state.legacy === "";
    setSynced({ q: state.q, legacy: state.legacy });
    if (!(translationLanded && draft.trim() !== "")) setDraft(state.q);
  }
  const edited = draft.trim() !== state.q;
  // One guard for the button and the key. A blank box runs nothing: there
  // is nothing to ask — a refetch would send `q=` for a 400 nobody sees —
  // and "run" must not quietly mean "clear the chart"; Clear says that.
  const canRun = draft.trim() !== "";
  const clear = () => {
    setDraft("");
    update({ q: "", legacy: "" });
  };
  const run = () => {
    if (!canRun) return;
    if (edited) update({ q: draft.trim() });
    else void query.refetch();
  };

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold">Metrics Explorer</h1>
      <MetricsTabs />

      <section aria-label="Query" className="flex flex-col gap-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
        <QueryEditor label="Query" value={draft} onChange={setDraft} variables={[]} onSubmit={run} />
        <div className="flex flex-wrap items-center gap-4">
          <button
            type="button"
            onClick={run}
            disabled={!canRun}
            className="rounded-md bg-violet-600 px-3 py-1 text-sm font-medium text-white hover:bg-violet-700 disabled:opacity-40"
          >
            Run
          </button>
          {state.q !== "" || state.legacy !== "" ? (
            <button
              type="button"
              onClick={clear}
              className="rounded-md border border-zinc-300 px-3 py-1 text-sm hover:bg-zinc-50 dark:border-zinc-700 dark:hover:bg-zinc-800"
            >
              Clear
            </button>
          ) : null}
          {edited && canRun && state.q !== "" ? (
            <span role="status" className="text-xs text-amber-700 dark:text-amber-400">
              Edited, not run: the chart is still the query in the URL.
            </span>
          ) : null}
          {!canRun && state.q !== "" ? (
            <span role="status" className="text-xs text-zinc-500">
              The box is empty; the chart is still the query in the URL. Clear removes it.
            </span>
          ) : null}
          <TimeRangePicker key={JSON.stringify(state.range)} range={state.range} onChange={(range) => update({ range })} />
          <label
            className="flex items-center gap-2 text-sm"
            title={state.range.kind === "absolute" ? "A fixed time window doesn't change, so there is nothing to refresh." : undefined}
          >
            <input
              type="checkbox"
              checked={autoRefresh}
              disabled={state.range.kind === "absolute"}
              onChange={(e) => update({ live: e.target.checked })}
            />
            Auto-refresh every {REFRESH_INTERVAL_MS / 1000}s
          </label>
        </div>
        <SaveToDashboard q={state.q} edited={edited && canRun} />
      </section>

      <section aria-label="Chart" className="flex flex-col gap-3 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
        {state.q === "" && state.legacy !== "" ? (
          <LegacyArea legacy={state.legacy} translation={legacy} />
        ) : (
          <ChartArea q={state.q} query={query} />
        )}
      </section>
    </div>
  );
}

interface ChartAreaProps {
  q: string;
  query: ReturnType<typeof useExplorerQuery>;
}

/**
 * The chart, or which of the ways of having no chart this is.
 *
 * Each is its own sentence because each means something different to the
 * reader: nothing asked, asked and waiting, refused (the query's fault, and
 * asking again will not help), not answered (ozyd's or the network's, and it
 * might), answered with nothing, or an answer that is not to this question —
 * the previous query's, kept on screen while this one runs. That last one is
 * drawn dimmed and says whose it is, because a chart under a query box reads
 * as that query's chart.
 */
function ChartArea({ q, query }: ChartAreaProps) {
  if (!q) {
    return <p className="text-zinc-500">Write a query and run it (Ctrl+Enter) to chart it.</p>;
  }
  const answer = query.data;
  const current = answer?.asked === q ? answer : undefined;
  if (query.error && !current) return <Failure error={query.error} />;
  if (!answer) {
    return (
      <p role="status" className="text-zinc-500">
        Running…
      </p>
    );
  }
  if (!current) {
    return (
      <>
        <p role="status" className="text-xs text-zinc-500">
          Running… the chart below is the previous query's answer, <code className="font-mono">{answer.asked}</code>.
        </p>
        <div className="opacity-40">
          <Answer answer={answer} />
        </div>
      </>
    );
  }
  return (
    <>
      {query.error ? (
        // TanStack keeps the last answer when a refresh fails, and that
        // answer is still true of its own window — so it stays, with the
        // time it was true at, rather than the page blanking or pretending.
        <p role="alert" className="text-sm text-red-700 dark:text-red-400">
          The last refresh failed ({query.error.message}). Showing the answer from{" "}
          {new Date(query.dataUpdatedAt).toLocaleTimeString()}.
        </p>
      ) : null}
      {query.isPlaceholderData ? (
        <p role="status" className="text-xs text-zinc-500">
          Updating for the new time range…
        </p>
      ) : null}
      <div className={query.isPlaceholderData ? "opacity-40" : undefined}>
        <Answer answer={current} />
      </div>
    </>
  );
}

/**
 * An M1 link on its way to being a `q` link, or stopped: translating, refused
 * by the server (the parameters stay in the URL, and the message is the
 * server's), not answered, or answered without saying what it ran — which
 * would otherwise leave a link that charts nothing and says nothing.
 */
function LegacyArea({ legacy, translation }: { legacy: string; translation: ReturnType<typeof useLegacyTranslation> }) {
  const shown = [...new URLSearchParams(legacy)].map(([k, v]) => `${k}=${v}`).join("&");
  const params = <code className="font-mono">{shown}</code>;
  // Not "run it again": the box is empty, and nothing on this page asks
  // again without a reload.
  if (translation.error)
    return <Failure error={translation.error} what={<>this link's M1 parameters, {params}</>} retry="Reload the page to ask again." />;
  if (translation.data && !translation.data.query)
    return (
      <p role="alert" className="text-sm text-red-700 dark:text-red-400">
        ozyd answered this link's M1 parameters, {params}, without saying what query they mean, so there is nothing to put in the box.
      </p>
    );
  return (
    <p role="status" className="text-zinc-500">
      Asking ozyd what this link's M1 parameters, {params}, mean as a query…
    </p>
  );
}

function Failure({
  error,
  what = "this query",
  retry = "Run it again to retry.",
}: {
  error: Error;
  what?: ReactNode;
  /** How to ask again on this page, said only when asking again might help. */
  retry?: string;
}) {
  // 400 is the query's fault: asking again will get the same answer. Anything
  // else — 503 out of time, 500, no answer at all — might not.
  const refused = error instanceof ApiError && error.status === 400;
  return (
    <div role="alert" className="text-sm text-red-700 dark:text-red-400">
      <p className="font-medium">
        {refused ? "ozyd refused " : "ozyd did not answer "}
        {what}:
      </p>
      <p className="whitespace-pre-wrap font-mono text-xs">{error.message}</p>
      {refused ? null : <p className="mt-1 text-xs text-zinc-500">{retry}</p>}
    </div>
  );
}

function Answer({ answer }: { answer: ExplorerAnswer }) {
  const { result } = answer;
  return (
    <div className="flex flex-col gap-2">
      {result.warnings.length ? (
        <ul aria-label="Warnings" className="list-disc pl-5 text-xs text-amber-700 dark:text-amber-400">
          {result.warnings.map((w, i) => (
            // Indexed: nothing promises two warnings differ.
            <li key={`${i}-${w}`}>{w}</li>
          ))}
        </ul>
      ) : null}
      {result.series.length === 0 ? (
        <p className="text-zinc-500">No data for this query in the selected time range.</p>
      ) : (
        <>
          <p className="text-xs text-zinc-500">
            {result.series.length} series · {result.interval}s buckets
            {result.query !== answer.asked ? (
              <>
                {" "}
                · evaluated as <code className="font-mono">{result.query}</code>
              </>
            ) : null}
          </p>
          <ResultChart result={result} />
          <Legend result={result} />
        </>
      )}
    </div>
  );
}

function ResultChart({ result }: { result: QueryResult }) {
  const data = useMemo(() => alignSeries(result.series), [result]);
  const labels = useMemo(() => result.series.map(seriesLabel), [result]);
  // Start at the first bucket, not at result.from: the server floors the range
  // start to the interval, so whenever from % interval != 0 the first bucket
  // begins *before* from and would fall outside the scale and never be drawn.
  const xRange = useMemo((): [number, number] => {
    const first = data[0]?.[0];
    return [first !== undefined ? first : result.from, result.to];
  }, [data, result]);
  return <TimeseriesChart data={data} labels={labels} xRange={xRange} height={320} />;
}

const LEGEND_REDUCERS = ["last", "avg", "min", "max"] as const satisfies readonly Reducer[];

/**
 * One row per line, with the numbers a reader otherwise hovers for. A line
 * with no value in the window shows dashes, not zeros: nothing was measured,
 * which is not the same as measuring nothing.
 */
function Legend({ result }: { result: QueryResult }) {
  return (
    <table aria-label="Series" className="w-full text-left text-xs tabular-nums">
      <thead className="text-zinc-500">
        <tr>
          <th className="py-1 font-medium">Series</th>
          {LEGEND_REDUCERS.map((r) => (
            <th key={r} className="py-1 text-right font-medium">
              {r}
            </th>
          ))}
        </tr>
      </thead>
      <tbody>
        {result.series.map((s, i) => {
          const label = seriesLabel(s);
          const values = s.points.map((p) => p[1]);
          return (
            <tr key={`${label}-${i}`} className="border-t border-zinc-100 dark:border-zinc-800">
              <td className="py-1 font-mono">{label}</td>
              {LEGEND_REDUCERS.map((r) => (
                <td key={r} className="py-1 text-right">
                  {formatNumber(reduceSeries(values, r))}
                </td>
              ))}
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}

function formatNumber(v: number | null): string {
  return v === null ? "—" : formatValue(v);
}
