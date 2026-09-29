import { useState } from "react";
import { Link } from "react-router";
import { useDashboardList } from "../../lib/useDashboards";

/** Props for SaveToDashboard. */
export interface SaveToDashboardProps {
  /** The charted query — what is saved; "" when nothing has been run. */
  q: string;
  /** Whether the box holds edits that have not been run. */
  edited: boolean;
}

const NEW = "new";

/**
 * "Save to dashboard": choose a dashboard, and open it in the editor with a
 * widget for the charted query added and unsaved (`?add=`, ADR-0022).
 *
 * Nothing is written from here. The editor shows where the widget lands and
 * saves it through the one save path, with every answer that path gives —
 * refused, conflict, deleted meanwhile, no answer — instead of this page
 * growing a second, smaller set of them.
 *
 * It saves the query that is *charted*, not the draft in the box: the chart
 * is what the author has seen, and a draft may not even parse. When they
 * differ it says so.
 */
export function SaveToDashboard({ q, edited }: SaveToDashboardProps) {
  const list = useDashboardList();
  const [target, setTarget] = useState(NEW);
  const dashboards = list.data?.dashboards ?? [];
  // A dashboard deleted after it was chosen is answered by the editor, which
  // says it does not exist and offers nothing to save into — the list here is
  // not refetched while the page is open, so this cannot know sooner.
  const chosen = target === NEW ? undefined : dashboards.find((d) => String(d.id) === target);
  const href = chosen
    ? `/dashboards/${chosen.id}/edit?${new URLSearchParams({ add: q })}`
    : `/dashboards/new?${new URLSearchParams({ add: q })}`;

  return (
    <div className="flex flex-col gap-1 border-t border-zinc-100 pt-3 text-sm dark:border-zinc-800">
      <div className="flex flex-wrap items-center gap-2">
        <label htmlFor="save-target" className="text-zinc-600 dark:text-zinc-300">
          Save to dashboard
        </label>
        <select
          id="save-target"
          value={chosen ? target : NEW}
          onChange={(e) => setTarget(e.target.value)}
          className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
        >
          <option value={NEW}>A new dashboard</option>
          {dashboards.map((d) => (
            // Offered but not choosable: an edit to a provisioned dashboard is
            // undone at the next restart, and a missing row would read as
            // "it is not in the list" rather than "it cannot take this".
            <option key={d.id} value={String(d.id)} disabled={d.provisioned}>
              {d.title}
              {d.provisioned ? " (from a file, edit the file)" : ""}
            </option>
          ))}
        </select>
        {q ? (
          <Link to={href} className="rounded-md border border-zinc-300 px-3 py-1 hover:bg-zinc-50 dark:border-zinc-700 dark:hover:bg-zinc-800">
            Open in the editor
          </Link>
        ) : (
          <span className="text-xs text-zinc-500">Run a query first; the one charted is the one saved.</span>
        )}
      </div>
      <ListNote list={list} />
      {q && edited ? (
        <p className="text-xs text-zinc-500">
          Saves the charted query, <code className="font-mono">{q}</code>, not the unrun edit in the box.
        </p>
      ) : null}
    </div>
  );
}

function ListNote({ list }: { list: ReturnType<typeof useDashboardList> }) {
  if (list.data) {
    const n = list.data.unreadable.length;
    return n ? (
      <p className="text-xs text-zinc-500">
        {n} stored dashboard{n === 1 ? "" : "s"} could not be read, so {n === 1 ? "it is" : "they are"} not offered.
      </p>
    ) : null;
  }
  if (list.error)
    return (
      <p role="alert" className="text-xs text-red-700 dark:text-red-400">
        Could not list the dashboards ({list.error.message}); a new one still works.
      </p>
    );
  return <p className="text-xs text-zinc-500">Loading the dashboards…</p>;
}
