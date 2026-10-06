import { useQuery } from "@tanstack/react-query";
import { Link, useParams } from "react-router";
import { fetchResources, fetchServices, fetchTraces, type RedRow } from "../../lib/apmApi";
import { apmWindow, traceSearchLink } from "../../lib/apmState";
import { fmtCount, fmtMs, fmtPct, fmtRate } from "../../lib/apmView";
import { rangeKey } from "../../lib/useMetricsApi";
import { ApmHeader, BTN, ErrorNote, scopeSearch, useApmState } from "./ApmShell";
import { RedTable } from "./RedTable";
import { Sparkline } from "./Sparkline";
import { TraceList } from "./TraceList";

const noop = () => {};

function Stat({ label, value, tone }: { label: string; value: string; tone?: "bad" }) {
  return (
    <div>
      <dt className="text-xs text-zinc-500">{label}</dt>
      <dd className={`text-lg font-semibold tabular-nums ${tone === "bad" ? "text-red-600 dark:text-red-400" : ""}`}>{value}</dd>
    </div>
  );
}

/** One card per entry-span kind: kinds are not merged because their latencies are different operations. */
function KindCard({ row }: { row: RedRow }) {
  return (
    <section aria-label={`${row.name} summary`} className="rounded-lg border border-zinc-200 p-3 dark:border-zinc-800">
      <h3 className="mb-2 text-sm font-medium text-zinc-600 dark:text-zinc-400">
        {row.name}
        {row.env ? ` · ${row.env}` : ""}
      </h3>
      <dl className="flex flex-wrap gap-x-6 gap-y-2">
        <Stat label="Requests" value={fmtCount(row.requests)} />
        <Stat label="Rate" value={fmtRate(row.requests_per_second)} />
        <Stat label="Errors" value={fmtPct(row.error_pct)} tone={row.errors > 0 ? "bad" : undefined} />
        <Stat label="p50" value={fmtMs(row.p50_ms)} />
        <Stat label="p95" value={fmtMs(row.p95_ms)} />
        <Stat label="p99" value={fmtMs(row.p99_ms)} />
        <div className="ml-auto self-end">
          <Sparkline values={row.sparkline} width={180} height={36} label={`${row.service} ${row.name} requests`} />
        </div>
      </dl>
    </section>
  );
}

/**
 * One service: the headline numbers per entry-span kind, its resources
 * (routes, jobs) with their own RED rows, and its most recent failed traces.
 * The numbers are from metrics (exact); the error traces are from the trace
 * store (a sample), which is why the list says "kept" and the card does not.
 * A resource row opens the trace search for it. A latency distribution
 * histogram from the sketch bins is not drawn: the API serves percentiles,
 * not bins, so the page shows what it can state truthfully.
 */
export function ServicePage() {
  const { service = "" } = useParams();
  const { state, update } = useApmState();
  const rk = rangeKey(state.range);
  const kinds = useQuery({
    queryKey: ["apm", "services", state.env, rk],
    queryFn: ({ signal }) => fetchServices(state.env, apmWindow(state.range, Date.now()), fetch, signal),
    retry: false,
  });
  const resources = useQuery({
    queryKey: ["apm", "resources", service, state.env, rk],
    queryFn: ({ signal }) => fetchResources(service, state.env, apmWindow(state.range, Date.now()), fetch, signal),
    retry: false,
  });
  const errors = useQuery({
    queryKey: ["apm", "traces", "errors", service, state.env, rk],
    queryFn: ({ signal }) => fetchTraces({ service, env: state.env || undefined, error: true }, apmWindow(state.range, Date.now()), { limit: 10 }, fetch, signal),
    retry: false,
  });
  const mine = (kinds.data?.services ?? []).filter((r) => r.service === service);
  const error = kinds.error ?? resources.error;

  return (
    <div className="flex flex-col gap-4">
      <ApmHeader title={service} state={state} update={update}>
        <button
          type="button"
          className={BTN}
          onClick={() => void Promise.all([kinds.refetch(), resources.refetch(), errors.refetch()])}
        >
          Refresh
        </button>
      </ApmHeader>
      <p className="text-sm">
        <Link to={{ pathname: "/apm", search: scopeSearch(state) }} className="text-violet-700 hover:underline dark:text-violet-300">
          ← All services
        </Link>
      </p>
      {error && <ErrorNote error={error} />}

      {kinds.isPending && !error ? (
        <p className="p-4 text-zinc-500">Loading…</p>
      ) : kinds.data && mine.length === 0 ? (
        <p className="p-4 text-center text-zinc-500">No requests for “{service}” in this window.</p>
      ) : (
        <div className="grid gap-3 lg:grid-cols-2">
          {mine.map((r) => (
            <KindCard key={`${r.env ?? ""}|${r.name}`} row={r} />
          ))}
        </div>
      )}

      <section aria-label="Resources">
        <h2 className="mb-1 font-semibold">Resources</h2>
        {resources.isPending && !resources.error ? (
          <p className="p-4 text-zinc-500">Loading resources…</p>
        ) : resources.data && resources.data.services.length === 0 ? (
          <p className="p-4 text-zinc-500">No resources recorded.</p>
        ) : resources.data ? (
          <RedTable rows={resources.data.services} mode="resource" state={state} service={service} />
        ) : null}
      </section>

      <section aria-label="Recent error traces">
        <div className="mb-1 flex items-baseline justify-between">
          <h2 className="font-semibold">Recent error traces</h2>
          <Link to={traceSearchLink(state, { service, error: true })} className="text-sm text-violet-700 hover:underline dark:text-violet-300">
            Search all
          </Link>
        </div>
        {errors.isPending ? (
          <p className="p-4 text-zinc-500">Loading traces…</p>
        ) : errors.error ? (
          <ErrorNote error={errors.error} />
        ) : (
          <TraceList traces={errors.data?.traces ?? []} onNearEnd={noop} height={Math.min(280, Math.max(1, errors.data?.traces.length ?? 1) * 28)} empty="No failed traces were kept in this window." />
        )}
        <p className="mt-1 text-xs text-zinc-500">Only traces the sampler kept are listed; the error rate above counts every request.</p>
      </section>
    </div>
  );
}
