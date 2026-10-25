/**
 * The monitor list: every monitor with its worst status and how many groups
 * are in each, filterable by name, tag and status, with mute one click away.
 *
 * Sorted worst first, then by name: the list is opened during an incident to
 * find what is red, and the server's order (by name) would bury it. A monitor
 * with no status (no data yet, disabled, or not running) sorts below OK, and
 * says which of the three it is: none of them is "fine".
 *
 * The filters live in the URL, so a link to "everything alerting for team:shop"
 * can be pasted into an incident channel.
 */
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link, useSearchParams } from "react-router";
import { muteMonitor, STATUSES, unmuteMonitor, type Status, type StoredMonitor } from "../../lib/monitorsApi";
import { muteLabel, sortMonitors } from "../../lib/monitorView";
import { useDebouncedValue } from "../../lib/useDebouncedValue";
import { MONITORS_KEY, useMonitorList } from "../../lib/useMonitors";
import { button, ErrorText, GroupCounts, input, primary, StatusPill } from "./parts";

/** Mute and unmute from the list. */
function MuteButton({ m, now }: { m: StoredMonitor; now: number }) {
  const client = useQueryClient();
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const muted = muteLabel(m.muted_until, now) !== null;
  const act = async () => {
    setBusy(true);
    setError(null);
    try {
      await (muted ? unmuteMonitor(m.id) : muteMonitor(m.id, Math.floor(Date.now() / 1000) + 3600));
      await client.invalidateQueries({ queryKey: [MONITORS_KEY] });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <span className="flex items-center gap-1">
      <button type="button" className={button} disabled={busy} onClick={() => void act()}
        title={muted ? "Unmute: notifications resume" : "Mute for an hour: evaluation continues, notifications stop"}>
        {muted ? "Unmute" : "Mute 1h"}
      </button>
      {error ? <span role="alert" className="text-xs text-red-700 dark:text-red-400">{error}</span> : null}
    </span>
  );
}

/** Lists the monitors. */
export function MonitorsIndex() {
  const [params, setParams] = useSearchParams();
  const q = params.get("q") ?? "";
  const tag = params.get("tag") ?? "";
  const status = (STATUSES as readonly string[]).includes(params.get("status") ?? "") ? (params.get("status") as Status) : undefined;
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
  };
  // Typed filters are asked about once typing pauses, not per keystroke.
  // Each string on its own: an object literal is new every render, which
  // would restart the timer forever.
  const askedQ = useDebouncedValue(q, 250);
  const askedTag = useDebouncedValue(tag, 250);
  const list = useMonitorList({ q: askedQ || undefined, tag: askedTag || undefined, status });
  const rows = useMemo(() => (list.data ? sortMonitors(list.data) : undefined), [list.data]);
  const filtered = Boolean(q || tag || status);
  // "Now" for the mute wording is when the list was fetched: render stays a
  // function of the data, and the list refetches every 15 seconds.
  const now = list.dataUpdatedAt / 1000;

  return (
    <div className="flex flex-col gap-6">
      <div className="flex items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">Monitors</h1>
        <Link to="/monitors/new" className={primary}>
          New monitor
        </Link>
      </div>

      <div className="flex flex-wrap items-end gap-2 text-sm">
        <label className="flex flex-col gap-0.5">
          <span className="text-xs text-zinc-500">Name</span>
          <input aria-label="Filter by name" className={input} value={q} onChange={(e) => set("q", e.target.value)} placeholder="contains…" />
        </label>
        <label className="flex flex-col gap-0.5">
          <span className="text-xs text-zinc-500">Tag</span>
          <input aria-label="Filter by tag" className={input} value={tag} onChange={(e) => set("tag", e.target.value.trim())} placeholder="team:shop" />
        </label>
        <label className="flex flex-col gap-0.5">
          <span className="text-xs text-zinc-500">Status</span>
          <select aria-label="Filter by status" className={input} value={status ?? ""} onChange={(e) => set("status", e.target.value)}>
            <option value="">any</option>
            {STATUSES.map((s) => (
              <option key={s} value={s}>
                has a group in {s}
              </option>
            ))}
          </select>
        </label>
      </div>

      {list.isPending ? <p className="text-sm text-zinc-500">Loading…</p> : null}
      {list.error ? <ErrorText>{list.error.message}</ErrorText> : null}
      {rows?.length === 0 ? (
        <p className="text-sm text-zinc-500">
          {filtered ? "No monitor matches these filters." : "No monitors yet. Create one, or provision them from provisioning.monitor_paths."}
        </p>
      ) : null}
      {rows && rows.length > 0 ? (
        <table className="w-full text-left text-sm">
          <thead className="text-xs text-zinc-500">
            <tr>
              <th className="py-1 pr-2 font-normal">Status</th>
              <th className="py-1 pr-2 font-normal">Name</th>
              <th className="py-1 pr-2 font-normal">Groups</th>
              <th className="py-1 pr-2 font-normal">Tags</th>
              <th className="py-1 font-normal">
                <span className="sr-only">Actions</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {rows.map((m) => (
              <tr key={m.id} className="border-t border-zinc-200 align-top dark:border-zinc-800">
                <td className="py-2 pr-2">
                  <StatusPill status={m.status} monitor={m} />
                </td>
                <td className="py-2 pr-2">
                  <Link to={`/monitors/${m.id}`} className="font-medium hover:underline">
                    {m.name}
                  </Link>
                  <div className="flex flex-wrap gap-1 text-xs text-zinc-500">
                    {m.provisioned ? <span title="Provisioned from a file: read-only here">from file</span> : null}
                    {muteLabel(m.muted_until, now) ? <span>{muteLabel(m.muted_until, now)}</span> : null}
                    {m.problem ? <span className="text-amber-700 dark:text-amber-500">{m.problem}</span> : null}
                  </div>
                </td>
                <td className="py-2 pr-2">
                  <GroupCounts groups={m.groups} />
                </td>
                <td className="py-2 pr-2 text-xs text-zinc-500">{m.tags.join(" ")}</td>
                <td className="py-2">
                  <MuteButton m={m} now={now} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </div>
  );
}
