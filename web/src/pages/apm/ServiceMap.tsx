import { useQuery } from "@tanstack/react-query";
import { useMemo } from "react";
import { Link } from "react-router";
import { fetchServiceMap, type MapEdge } from "../../lib/apmApi";
import { apmWindow } from "../../lib/apmState";
import { formatDuration } from "../../lib/flame";
import { rangeKey } from "../../lib/useMetricsApi";
import { ApmHeader, BTN, ErrorNote, scopeSearch, useApmState } from "./ApmShell";

const COL_W = 220;
const ROW_H = 70;
const NODE_W = 150;
const NODE_H = 36;

/**
 * Columns by call depth: a service's column is the longest chain of callers
 * above it, so calls read left to right. Cycles (a callback) would never
 * settle, so the pass count is bounded by the node count.
 */
export function columns(nodes: readonly string[], edges: readonly Pick<MapEdge, "parent" | "child">[]): Map<string, number> {
  const col = new Map(nodes.map((n) => [n, 0]));
  for (let pass = 0; pass < nodes.length; pass++) {
    let changed = false;
    for (const e of edges) {
      const want = (col.get(e.parent) ?? 0) + 1;
      if (e.parent !== e.child && want > (col.get(e.child) ?? 0) && want < nodes.length) {
        col.set(e.child, want);
        changed = true;
      }
    }
    if (!changed) break;
  }
  return col;
}

/** The service map: who calls whom. Width is call count, red is error share. Counts are from stored (sampled) spans: shape, not rate. */
export function ServiceMap() {
  const { state, update } = useApmState();
  const q = useQuery({
    queryKey: ["apm", "map", state.env, rangeKey(state.range)],
    queryFn: ({ signal }) => fetchServiceMap(state.env, apmWindow(state.range, Date.now()), fetch, signal),
    retry: false,
  });
  const g = useMemo(() => {
    if (!q.data) return undefined;
    const col = columns(q.data.nodes.map((n) => n.service), q.data.edges);
    const perCol = new Map<number, number>();
    const pos = new Map<string, { x: number; y: number }>();
    for (const n of q.data.nodes) {
      const c = col.get(n.service) ?? 0;
      const r = perCol.get(c) ?? 0;
      perCol.set(c, r + 1);
      pos.set(n.service, { x: 20 + c * COL_W, y: 20 + r * ROW_H });
    }
    const width = 40 + (Math.max(0, ...col.values()) + 1) * COL_W;
    const height = 40 + Math.max(1, ...perCol.values()) * ROW_H;
    return { pos, width, height, max: Math.max(1, ...q.data.edges.map((e) => e.calls)) };
  }, [q.data]);

  return (
    <div className="flex flex-col gap-3">
      <ApmHeader title="APM" state={state} update={update}>
        <button type="button" className={BTN} onClick={() => void q.refetch()}>
          Refresh
        </button>
      </ApmHeader>
      {q.error && <ErrorNote error={q.error as Error} />}
      {q.data && q.data.nodes.length === 0 && <p className="p-6 text-center text-zinc-500">No services have sent traces in this window.</p>}
      {q.data && g && q.data.nodes.length > 0 && (
        <>
          <svg role="img" aria-label="Service map" width={g.width} height={g.height} className="max-w-full overflow-visible rounded-lg border border-zinc-200 dark:border-zinc-800">
            {q.data.edges.map((e) => {
              const a = g.pos.get(e.parent);
              const b = g.pos.get(e.child);
              if (!a || !b) return null;
              const share = e.calls ? e.errors / e.calls : 0;
              return (
                <g key={`${e.parent}>${e.child}`}>
                  <title>{`${e.parent} → ${e.child}: ${e.calls} calls, ${e.errors} errors, avg ${formatDuration(e.avg_duration)}`}</title>
                  <line
                    x1={a.x + NODE_W}
                    y1={a.y + NODE_H / 2}
                    x2={b.x}
                    y2={b.y + NODE_H / 2}
                    strokeWidth={1 + (e.calls / g.max) * 7}
                    stroke={share > 0 ? `rgb(${Math.round(120 + 100 * share)},${Math.round(113 - 80 * share)},${Math.round(133 - 90 * share)})` : "#a1a1aa"}
                    opacity={0.8}
                  />
                </g>
              );
            })}
            {q.data.nodes.map((n) => {
              const p = g.pos.get(n.service)!;
              return (
                <g key={n.service}>
                  <rect x={p.x} y={p.y} width={NODE_W} height={NODE_H} rx={6} className="fill-white stroke-zinc-400 dark:fill-zinc-900" />
                  <text x={p.x + NODE_W / 2} y={p.y + 22} textAnchor="middle" className="fill-current text-xs">
                    {n.service.length > 20 ? `${n.service.slice(0, 19)}…` : n.service}
                  </text>
                </g>
              );
            })}
          </svg>
          <ul className="flex flex-wrap gap-3 text-sm">
            {q.data.nodes.map((n) => (
              <li key={n.service}>
                <Link to={{ pathname: `/apm/services/${encodeURIComponent(n.service)}`, search: scopeSearch(state) }} className="text-violet-700 hover:underline dark:text-violet-300">
                  {n.service}
                </Link>
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}
