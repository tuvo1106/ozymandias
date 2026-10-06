import { useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { fetchTrace, type TraceSpan } from "../../lib/apmApi";
import { traceLink } from "../../lib/apmState";
import { FULL_VIEW, formatDuration, layoutTrace, serviceColor, type View } from "../../lib/flame";
import { fetchLogs } from "../../lib/logsApi";
import { BTN, ErrorNote } from "./ApmShell";
import { FlameCanvas } from "./FlameCanvas";

type Mode = "flame" | "waterfall";
type Tab = "spans" | "logs";

/** Span tags worth showing first; the rest follow alphabetically. */
const FIRST = ["http.method", "http.route", "http.status_code", "db.system", "queue.name", "job.id", "span.kind"];

function Detail({ span, offset }: { span: TraceSpan; offset: number }) {
  const meta = Object.entries(span.meta ?? {}).sort(([a], [b]) => (FIRST.indexOf(a) + 100) % 100 - (FIRST.indexOf(b) + 100) % 100 || a.localeCompare(b));
  return (
    <aside aria-label="Span detail" className="rounded-lg border border-zinc-200 p-3 text-sm dark:border-zinc-800">
      <h3 className="font-medium">
        {span.name} <span className="text-zinc-500">· {span.service}</span>
      </h3>
      <p className="mt-1 break-all font-mono text-xs">{span.resource}</p>
      <dl className="mt-2 grid grid-cols-[8rem_1fr] gap-x-2 gap-y-1">
        <dt className="text-zinc-500">duration</dt>
        <dd>{formatDuration(span.duration)}</dd>
        <dt className="text-zinc-500">starts at</dt>
        <dd>+{formatDuration(offset)}</dd>
        <dt className="text-zinc-500">type</dt>
        <dd>{span.type}</dd>
        {meta.map(([k, v]) => (
          <div key={k} className="contents">
            <dt className="text-zinc-500">{k}</dt>
            <dd className={k === "error.stack" ? "whitespace-pre-wrap break-all font-mono text-xs" : "break-all"}>{v}</dd>
          </div>
        ))}
        {Object.entries(span.metrics ?? {})
          .filter(([k]) => !k.startsWith("_"))
          .map(([k, v]) => (
            <div key={k} className="contents">
              <dt className="text-zinc-500">{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
      </dl>
    </aside>
  );
}

/**
 * One trace: flame graph or waterfall, a span detail panel and the trace's own
 * logs. The selected span and tab live in the URL so a link shows what you saw.
 */
export function TraceView() {
  const { traceId = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const selected = params.get("span") ?? undefined;
  const tab: Tab = params.get("tab") === "logs" ? "logs" : "spans";
  const [mode, setMode] = useState<Mode>("flame");
  const [view, setView] = useState<View>(FULL_VIEW);
  const [critical, setCritical] = useState(false);
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set());

  const q = useQuery({ queryKey: ["apm", "trace", traceId], queryFn: ({ signal }) => fetchTrace(traceId, fetch, signal), retry: false });
  const layout = useMemo(() => (q.data ? layoutTrace(q.data.spans, { collapsed }) : undefined), [q.data, collapsed]);
  const logs = useQuery({
    queryKey: ["apm", "trace-logs", traceId],
    enabled: tab === "logs" && !!q.data,
    queryFn: ({ signal }) =>
      fetchLogs({ q: `trace_id:${traceId}`, from: Math.floor(q.data!.start / 1000) - 3_600_000, to: Math.floor((q.data!.start + q.data!.duration) / 1000) + 3_600_000 }, { limit: 200 }, fetch, signal),
    retry: false,
  });

  const select = (id: string | undefined) =>
    setParams((p) => {
      const n = new URLSearchParams(p);
      if (id && !id.startsWith("missing:")) n.set("span", id);
      else n.delete("span");
      return n;
    });
  const setTab = (t: Tab) =>
    setParams((p) => {
      const n = new URLSearchParams(p);
      if (t === "logs") n.set("tab", "logs");
      else n.delete("tab");
      return n;
    });
  const toggle = (id: string) =>
    setCollapsed((c) => {
      const n = new Set(c);
      if (!n.delete(id)) n.add(id);
      return n;
    });

  const span = q.data?.spans.find((s) => s.span_id === selected);
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Link to="/apm/traces" className="text-sm text-violet-700 hover:underline dark:text-violet-300">
          ← Traces
        </Link>
        <h1 className="font-mono text-lg">{traceId}</h1>
      </div>
      {q.isPending && <p className="text-zinc-500">Loading trace…</p>}
      {q.error && (
        <ErrorNote error={q.error as Error} />
      )}
      {q.data && layout && (
        <>
          <p className="text-sm text-zinc-600 dark:text-zinc-400">
            {formatDuration(q.data.duration)} · {q.data.span_count} spans · {q.data.services.join(", ")}
            {q.data.errors > 0 && <span className="text-red-600 dark:text-red-400"> · {q.data.errors} errors</span>}
            {layout.missing > 0 && <span> · {layout.missing} missing parent(s): their process was not sampled or has not flushed</span>}
            {layout.clamped > 0 && <span> · {layout.clamped} span(s) clamped into their parent (clock skew)</span>}
          </p>
          <div className="flex flex-wrap gap-2">
            <button type="button" className={BTN} aria-pressed={mode === "flame"} onClick={() => setMode("flame")}>
              Flame graph
            </button>
            <button type="button" className={BTN} aria-pressed={mode === "waterfall"} onClick={() => setMode("waterfall")}>
              Waterfall
            </button>
            <button type="button" className={BTN} aria-pressed={critical} onClick={() => setCritical((c) => !c)}>
              Critical path
            </button>
            <button type="button" className={BTN} onClick={() => setView(FULL_VIEW)}>
              Reset zoom
            </button>
            <span className="ml-auto flex gap-1">
              <button type="button" className={BTN} aria-pressed={tab === "spans"} onClick={() => setTab("spans")}>
                Spans
              </button>
              <button type="button" className={BTN} aria-pressed={tab === "logs"} onClick={() => setTab("logs")}>
                Logs
              </button>
            </span>
          </div>
          {tab === "logs" ? (
            <section aria-label="Trace logs" className="rounded-lg border border-zinc-200 p-3 font-mono text-xs dark:border-zinc-800">
              {logs.isPending && <p className="text-zinc-500">Loading logs…</p>}
              {logs.error && <ErrorNote error={logs.error as Error} />}
              {logs.data?.logs.length === 0 && <p className="text-zinc-500">No logs carry this trace id.</p>}
              {logs.data?.logs.map((l, i) => (
                <div key={i} className="flex gap-2 py-0.5">
                  <span className="text-zinc-500">{new Date(l.ts).toISOString().slice(11, 23)}</span>
                  <span className={l.status === "error" || l.status === "critical" ? "text-red-600 dark:text-red-400" : ""}>{l.status}</span>
                  <span className="break-all">{l.message}</span>
                </div>
              ))}
              <Link to={`/logs?q=${encodeURIComponent(`trace_id:${traceId}`)}`} className="mt-2 inline-block text-violet-700 hover:underline dark:text-violet-300">
                Open in Log Explorer
              </Link>
            </section>
          ) : mode === "flame" ? (
            <FlameCanvas layout={layout} view={view} onView={setView} selected={selected} onSelect={select} critical={critical} />
          ) : (
            <ol aria-label="Waterfall" className="rounded-lg border border-zinc-200 text-xs dark:border-zinc-800">
              {layout.rects.map((r) => {
                const gap = layout.gaps.find((g) => g.jobId === r.id);
                return (
                  <li key={r.id} className={`flex items-center gap-2 px-2 py-0.5 ${r.id === selected ? "bg-violet-50 dark:bg-violet-500/10" : ""} ${critical && !r.critical ? "opacity-40" : ""}`}>
                    <button type="button" className="w-64 shrink-0 truncate text-left" style={{ paddingLeft: r.level * 12 }} onClick={() => select(r.id)}>
                      {r.span ? `${r.span.name} · ${r.span.resource}` : "missing parent"}
                    </button>
                    {r.childCount > 0 && (
                      <button type="button" aria-label={r.collapsed ? "Expand" : "Collapse"} className="w-5 text-zinc-500" onClick={() => toggle(r.id)}>
                        {r.collapsed ? `+${r.hidden}` : "−"}
                      </button>
                    )}
                    <div className="relative h-4 flex-1">
                      {gap && (
                        <div
                          title={`queue wait ${formatDuration(gap.waitUs)}`}
                          className="absolute top-1 h-2"
                          style={{ left: `${gap.x * 100}%`, width: `${gap.w * 100}%`, backgroundImage: "repeating-linear-gradient(45deg, #a1a1aa 0 3px, transparent 3px 6px)" }}
                        />
                      )}
                      <div
                        className="absolute top-0 h-4 rounded-sm"
                        style={{ left: `${r.x * 100}%`, width: `max(2px, ${r.w * 100}%)`, background: r.error ? "#dc2626" : serviceColor(r.span?.service ?? "?") }}
                      />
                    </div>
                    <span className="w-16 shrink-0 text-right tabular-nums">{formatDuration(r.end - r.start)}</span>
                  </li>
                );
              })}
            </ol>
          )}
          {tab === "spans" && span && <Detail span={span} offset={span.start - layout.start} />}
          {tab === "spans" && !span && <p className="text-sm text-zinc-500">Select a span for its tags, error and statement.</p>}
          <p className="sr-only">
            <Link to={traceLink(traceId)}>permalink</Link>
          </p>
        </>
      )}
    </div>
  );
}
