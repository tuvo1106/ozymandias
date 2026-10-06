import { useQuery } from "@tanstack/react-query";
import { fetchServices } from "../../lib/apmApi";
import { apmWindow } from "../../lib/apmState";
import { rangeKey } from "../../lib/useMetricsApi";
import { ApmHeader, BTN, ErrorNote, useApmState } from "./ApmShell";
import { RedTable } from "./RedTable";

/**
 * The APM landing page: one RED row per service and entry-span kind.
 * Computed from the agent's `trace.*` metrics, which count every span before
 * sampling, so the table is exact even when the trace store keeps one trace
 * in ten (docs/api.md, "Tracing (APM)").
 */
export function ServiceList() {
  const { state, update } = useApmState();
  const q = useQuery({
    queryKey: ["apm", "services", state.env, rangeKey(state.range)],
    queryFn: ({ signal }) => fetchServices(state.env, apmWindow(state.range, Date.now()), fetch, signal),
    retry: false,
  });
  return (
    <div className="flex flex-col gap-3">
      <ApmHeader title="APM" state={state} update={update}>
        <button type="button" className={BTN} onClick={() => void q.refetch()}>
          Refresh
        </button>
      </ApmHeader>
      {q.error && <ErrorNote error={q.error} />}
      {q.isPending && !q.error ? (
        <p className="p-6 text-center text-zinc-500">Loading services…</p>
      ) : q.data && q.data.services.length === 0 ? (
        <p className="p-6 text-center text-zinc-500">
          No requests in this window{state.env ? ` for env “${state.env}”` : ""}. Services appear here once an instrumented app handles a request.
        </p>
      ) : q.data ? (
        <>
          <RedTable rows={q.data.services} mode="service" state={state} />
          <p className="text-xs text-zinc-500">Counted from every request before trace sampling. “—” means no latency was recorded in the window, not zero.</p>
        </>
      ) : null}
    </div>
  );
}
