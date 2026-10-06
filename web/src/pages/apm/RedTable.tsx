import { useState } from "react";
import { Link } from "react-router";
import type { RedRow } from "../../lib/apmApi";
import { traceSearchLink, type ApmState } from "../../lib/apmState";
import { fmtCount, fmtMs, fmtPct, fmtRate, rowLabel, sortRows, type SortKey } from "../../lib/apmView";
import { scopeSearch } from "./ApmShell";
import { Sparkline } from "./Sparkline";

/** Props for RedTable. */
export interface RedTableProps {
  rows: readonly RedRow[];
  /** `service`: rows link to the service page. `resource`: rows link to a trace search for that resource. */
  mode: "service" | "resource";
  state: ApmState;
  /** The service the resource rows belong to. */
  service?: string;
}

const COLUMNS: { key: SortKey; label: string; right?: boolean }[] = [
  { key: "requests", label: "Requests", right: true },
  { key: "rps", label: "Rate", right: true },
  { key: "error_pct", label: "Errors", right: true },
  { key: "p50", label: "p50", right: true },
  { key: "p95", label: "p95", right: true },
  { key: "p99", label: "p99", right: true },
];

/**
 * A requests / errors / latency table, sortable by any column, with a
 * sparkline of requests per row. Numbers come from metrics counted before
 * sampling, so they are exact; a latency cell is "—" when the window holds no
 * latency sketches, and such rows sort last whichever way the column is sorted.
 * The first column's label differs by mode only: a service row opens the
 * service page, a resource row opens the traces of that resource (the
 * resource tag is lower-cased, and the trace search ignores case, so the
 * link finds the span's own spelling).
 */
export function RedTable({ rows, mode, state, service }: RedTableProps) {
  const [sort, setSort] = useState<{ key: SortKey; dir: "asc" | "desc" }>(
    mode === "service" ? { key: "requests", dir: "desc" } : { key: "p95", dir: "desc" },
  );
  const sorted = sortRows(rows, sort.key, sort.dir);
  const head = (key: SortKey, label: string, right?: boolean) => (
    <th scope="col" aria-sort={sort.key === key ? (sort.dir === "asc" ? "ascending" : "descending") : "none"} className={`px-2 py-1 font-medium ${right ? "text-right" : "text-left"}`}>
      <button
        type="button"
        className="hover:underline"
        onClick={() => setSort((s) => ({ key, dir: s.key === key && s.dir === "desc" ? "asc" : "desc" }))}
      >
        {label}
        {sort.key === key && <span aria-hidden>{sort.dir === "asc" ? " ▲" : " ▼"}</span>}
      </button>
    </th>
  );
  return (
    <table className="w-full text-sm">
      <thead className="border-b border-zinc-200 text-zinc-500 dark:border-zinc-800">
        <tr>
          {head("name", mode === "service" ? "Service" : "Resource")}
          {COLUMNS.map((c) => head(c.key, c.label, c.right))}
          <th scope="col" className="px-2 py-1 text-left font-medium">
            Requests over time
          </th>
        </tr>
      </thead>
      <tbody>
        {sorted.map((r) => {
          const label = rowLabel(r);
          const to =
            mode === "service"
              ? { pathname: `/apm/services/${encodeURIComponent(r.service)}`, search: scopeSearch(state) }
              : traceSearchLink(state, { service: service ?? r.service, resource: r.resource });
          return (
            <tr key={`${r.service}|${r.env ?? ""}|${r.name}|${r.resource ?? ""}`} className="border-b border-zinc-100 hover:bg-zinc-50 dark:border-zinc-900 dark:hover:bg-zinc-900">
              <td className="px-2 py-1">
                <Link to={to} className="font-medium text-violet-700 hover:underline dark:text-violet-300">
                  {label}
                </Link>{" "}
                <span className="text-xs text-zinc-500">
                  {r.name}
                  {r.env ? ` · ${r.env}` : ""}
                </span>
              </td>
              <td className="px-2 py-1 text-right tabular-nums">{fmtCount(r.requests)}</td>
              <td className="px-2 py-1 text-right tabular-nums">{fmtRate(r.requests_per_second)}</td>
              <td className={`px-2 py-1 text-right tabular-nums ${r.errors > 0 ? "text-red-600 dark:text-red-400" : ""}`}>
                {fmtPct(r.error_pct)}
                <span className="ml-1 text-xs text-zinc-500">({fmtCount(r.errors)})</span>
              </td>
              <td className="px-2 py-1 text-right tabular-nums">{fmtMs(r.p50_ms)}</td>
              <td className="px-2 py-1 text-right tabular-nums">{fmtMs(r.p95_ms)}</td>
              <td className="px-2 py-1 text-right tabular-nums">{fmtMs(r.p99_ms)}</td>
              <td className="px-2 py-1">
                <Sparkline values={r.sparkline} label={`${label} requests`} />
              </td>
            </tr>
          );
        })}
      </tbody>
    </table>
  );
}
