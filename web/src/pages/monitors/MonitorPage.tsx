/**
 * One monitor: where it is now, how it got there, and who was told.
 *
 * - **Groups**: each group's state, worst first, with when it entered it.
 * - **Timeline**: each group's status over the last day, read backwards from
 *   its state through its transitions (lib/monitorView.ts).
 * - **Chart**: the last day replayed (the same backtest the editor runs), with
 *   markers at the transitions that *really* happened. Where the two disagree
 *   is worth looking at: the replay reads today's data, and late data or an
 *   edit since would explain a difference.
 * - **History** and the **notification log**: the events, newest first.
 *
 * Mute, unmute, test notification, edit and delete are here. Delete asks
 * twice in the page rather than through `window.confirm`, which blocks the
 * whole tab and every test of it.
 */
import { useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { Link, useNavigate, useParams } from "react-router";
import { transitionMarkers } from "../../lib/monitorChart";
import { definitionOf, formOf } from "../../lib/monitorForm";
import {
  deleteMonitor,
  muteMonitor,
  testNotification,
  unmuteMonitor,
  type MonitorEvent,
  type StoredMonitor,
  type TestResult,
} from "../../lib/monitorsApi";
import { describeEvent, muteLabel, NOTIFICATION_KINDS, statusTimeline } from "../../lib/monitorView";
import { MONITORS_KEY, useBacktest, useMonitor, useMonitorEvents, useMonitorStates, useValidation } from "../../lib/useMonitors";
import { formatValue } from "../../lib/chartData";
import { MonitorChart } from "./MonitorChart";
import { button, ErrorText, GroupCounts, StatusPill, TimelineBar } from "./parts";

const DAY = 86_400;

/** The detail page for /monitors/:id. */
export function MonitorPage() {
  const id = Number(useParams().id);
  const m = useMonitor(Number.isInteger(id) ? id : undefined);
  if (!Number.isInteger(id)) return <ErrorText>Not a monitor id.</ErrorText>;
  if (m.error) return <ErrorText>{m.error.message}</ErrorText>;
  if (!m.data) return <p className="text-sm text-zinc-500">Loading…</p>;
  // "Now" is when the monitor was fetched: render stays a function of the
  // data, and it refetches every 15 seconds.
  return <MonitorDetail m={m.data} now={m.dataUpdatedAt / 1000} />;
}

const time = (iso: string | null) => (iso ? new Date(iso).toLocaleString() : "never");

function Actions({ m, now }: { m: StoredMonitor; now: number }) {
  const client = useQueryClient();
  const navigate = useNavigate();
  const [error, setError] = useState<string | null>(null);
  const [results, setResults] = useState<TestResult[] | null>(null);
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [busy, setBusy] = useState(false);
  const run = async (f: () => Promise<unknown>) => {
    setBusy(true);
    setError(null);
    try {
      await f();
      await client.invalidateQueries({ queryKey: [MONITORS_KEY] });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };
  const muted = muteLabel(m.muted_until, now);
  // Read at the click, not at render.
  const at = (s: number) => Math.floor(Date.now() / 1000) + s;
  return (
    <div className="flex flex-col items-end gap-1">
      <div className="flex flex-wrap items-center justify-end gap-2">
        {muted ? (
          <button type="button" className={button} disabled={busy} onClick={() => void run(() => unmuteMonitor(m.id))}>
            Unmute
          </button>
        ) : (
          <>
            <button type="button" className={button} disabled={busy} onClick={() => void run(() => muteMonitor(m.id, at(3600)))}>
              Mute 1h
            </button>
            <button type="button" className={button} disabled={busy} onClick={() => void run(() => muteMonitor(m.id, at(DAY)))}>
              Mute 1d
            </button>
            <button type="button" className={button} disabled={busy} onClick={() => void run(() => muteMonitor(m.id))}>
              Mute
            </button>
          </>
        )}
        <button type="button" className={button} disabled={busy}
          onClick={() => void run(async () => setResults(await testNotification(m.id)))}>
          Test notification
        </button>
        <Link to={`/monitors/${m.id}/edit`} className={button}>
          {m.provisioned ? "View definition" : "Edit"}
        </Link>
        {m.provisioned ? null : confirmDelete ? (
          <>
            <button type="button" className={`${button} text-red-700 dark:text-red-400`} disabled={busy}
              onClick={() => void run(async () => {
                await deleteMonitor(m.id);
                navigate("/monitors");
              })}>
              Delete, with its history
            </button>
            <button type="button" className={button} onClick={() => setConfirmDelete(false)}>
              Keep
            </button>
          </>
        ) : (
          <button type="button" className={button} onClick={() => setConfirmDelete(true)}>
            Delete
          </button>
        )}
      </div>
      {error ? <ErrorText>{error}</ErrorText> : null}
      {results ? (
        <ul aria-label="Test results" className="text-xs">
          {results.map((r) => (
            <li key={r.notifier} className={r.ok ? "text-green-700 dark:text-green-400" : "text-red-700 dark:text-red-400"}>
              @{r.notifier}: {r.ok ? "sent" : r.error}
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

function MonitorDetail({ m, now }: { m: StoredMonitor; now: number }) {
  const states = useMonitorStates(m.id);
  const events = useMonitorEvents({ monitor_id: m.id, limit: 1000 }, DAY);
  const def = useMemo(() => {
    const r = definitionOf(formOf(m));
    return r.ok ? r.definition : undefined;
  }, [m]);
  // The same answers the editor uses: the levels for the bands, the replay
  // for the line. Not for a monitor that is not running: its definition is
  // what does not compile.
  const validation = useValidation(m.problem ? undefined : def);
  const valid = validation.data?.valid === true ? validation.data : undefined;
  const backtest = useBacktest(valid ? def : undefined, m.id);
  const timeline = useMemo(
    () => (states.data && events.data ? statusTimeline(events.data, states.data, now - DAY, now) : []),
    [states.data, events.data, now],
  );
  const markers = useMemo(() => (events.data ? transitionMarkers(events.data) : undefined), [events.data]);
  const muted = muteLabel(m.muted_until, now);

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div className="flex flex-col gap-1">
          <div className="flex items-center gap-2">
            <StatusPill status={m.status} monitor={m} />
            <h1 className="text-2xl font-semibold">{m.name}</h1>
          </div>
          <code className="text-xs text-zinc-500">{m.query}</code>
          <div className="flex flex-wrap gap-2 text-xs text-zinc-500">
            <GroupCounts groups={m.groups} />
            {muted ? <span>{muted}: evaluated, not notified</span> : null}
            {m.provisioned ? <span>provisioned from a file</span> : null}
            {m.tags.length ? <span>{m.tags.join(" ")}</span> : null}
          </div>
          {m.problem ? <ErrorText>Not running: {m.problem}</ErrorText> : null}
        </div>
        <Actions m={m} now={now} />
      </header>

      <section aria-label="Groups" className="flex flex-col gap-2">
        <h2 className="text-lg font-medium">Groups</h2>
        {states.error ? <ErrorText>{states.error.message}</ErrorText> : null}
        {states.data?.length === 0 ? (
          <p className="text-sm text-zinc-500">No group has had data yet.</p>
        ) : null}
        {states.data?.length ? (
          <table className="w-full text-left text-sm">
            <thead className="text-xs text-zinc-500">
              <tr>
                <th className="py-1 pr-2 font-normal">Group</th>
                <th className="py-1 pr-2 font-normal">Status</th>
                <th className="py-1 pr-2 font-normal">Since</th>
                <th className="py-1 pr-2 text-right font-normal">Value</th>
                <th className="py-1 pr-2 font-normal">Last data</th>
                <th className="py-1 font-normal">Last notified</th>
              </tr>
            </thead>
            <tbody>
              {states.data.map((s) => (
                <tr key={s.group} className="border-t border-zinc-200 dark:border-zinc-800">
                  <td className="py-1 pr-2 font-mono text-xs">{s.group}</td>
                  <td className="py-1 pr-2">
                    <StatusPill status={s.status} />
                  </td>
                  <td className="py-1 pr-2 text-xs">{time(s.since)}</td>
                  <td className="py-1 pr-2 text-right">{s.value === null ? "—" : formatValue(s.value)}</td>
                  <td className="py-1 pr-2 text-xs">{time(s.last_data)}</td>
                  <td className="py-1 text-xs">{time(s.last_notified)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        ) : null}
      </section>

      <section aria-label="Timeline" className="flex flex-col gap-2">
        <h2 className="text-lg font-medium">Last day</h2>
        {timeline.length > 0 ? (
          <div className="flex flex-col gap-1">
            {timeline.map((g) => (
              <div key={g.group} className="grid grid-cols-[10rem_1fr] items-center gap-2">
                <span className="truncate font-mono text-xs" title={g.group}>
                  {g.group}
                </span>
                <TimelineBar segments={g.segments} from={now - DAY} to={now} />
              </div>
            ))}
          </div>
        ) : null}
        {validation.data?.valid === false ? <ErrorText>{validation.data.errors.join("; ")}</ErrorText> : null}
        {backtest.error ? <ErrorText>{backtest.error.message}</ErrorText> : null}
        {backtest.data && valid ? (
          <>
            <MonitorChart backtest={backtest.data} levels={valid.levels} evalWindow={valid.window} markers={markers} />
            <p className="text-xs text-zinc-500">
              The line is the last day replayed with today&apos;s data; the markers are the transitions that were recorded.
            </p>
          </>
        ) : null}
      </section>

      <EventTable title="Notification log" events={events.data?.filter((e) => NOTIFICATION_KINDS.includes(e.kind))} error={events.error?.message} />
      <EventTable title="History" events={events.data} error={events.error?.message} />
    </div>
  );
}

function EventTable({ title, events, error }: { title: string; events: MonitorEvent[] | undefined; error?: string }) {
  return (
    <section aria-label={title} className="flex flex-col gap-2">
      <h2 className="text-lg font-medium">{title}</h2>
      {error ? <ErrorText>{error}</ErrorText> : null}
      {events?.length === 0 ? <p className="text-sm text-zinc-500">Nothing in the last day.</p> : null}
      {events?.length ? (
        <table className="w-full text-left text-sm">
          <tbody>
            {events.map((e) => (
              <tr key={e.id} className="border-t border-zinc-200 dark:border-zinc-800">
                <td className="py-1 pr-2 text-xs whitespace-nowrap">{time(e.ts)}</td>
                <td className="py-1 pr-2 font-mono text-xs">{e.group ?? ""}</td>
                <td className="py-1 pr-2">{describeEvent(e)}</td>
                <td className="py-1 text-right">{e.value === null ? "" : formatValue(e.value)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : null}
    </section>
  );
}
