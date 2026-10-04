import { useEffect, useRef, useState } from "react";
import { openTail, type LogEntry, type TailSource } from "./logsApi";
import { mergeTail } from "./logsView";

/** Most rows the tail keeps; older ones fall off. */
export const TAIL_CAP = 5000;
/** How often arrivals are flushed into state: one render per tick, not per log. */
export const TAIL_FLUSH_MS = 250;

/** What the tail hook reports. */
export interface LogTail {
  /** Tailed logs, newest first. */
  logs: LogEntry[];
  /** Logs the server dropped because this reader fell behind. */
  dropped: number;
  connected: boolean;
  paused: boolean;
  /** Arrivals held back while paused. */
  waiting: number;
  setPaused: (p: boolean) => void;
}

interface Snapshot {
  /** The (enabled, q) this snapshot belongs to; a different one is stale. */
  key: string;
  logs: LogEntry[];
  dropped: number;
  connected: boolean;
  paused: boolean;
  waiting: number;
}

const empty = (key: string): Snapshot => ({ key, logs: [], dropped: 0, connected: false, paused: false, waiting: 0 });

/**
 * Streams logs matching `q` while `enabled`. Arrivals are buffered and
 * flushed on a timer; while paused they stay in the buffer, so the list holds
 * still to be read and nothing is lost on resume. A change of `q` or of
 * `enabled` starts from nothing: a snapshot made for another key is ignored
 * rather than reset from inside an effect.
 */
export function useLogTail(q: string, enabled: boolean, make?: (url: string) => TailSource): LogTail {
  const key = enabled ? `on:${q}` : "off";
  const [snap, setSnap] = useState<Snapshot>(() => empty(key));
  const pending = useRef<LogEntry[]>([]);
  const paused = useRef(false);
  const cur = snap.key === key ? snap : empty(key);
  const isPaused = cur.paused;
  useEffect(() => {
    paused.current = isPaused;
  }, [isPaused]);

  useEffect(() => {
    if (!enabled) return;
    pending.current = [];
    const patch = (f: (s: Snapshot) => Partial<Snapshot>) => setSnap((s) => ({ ...(s.key === key ? s : empty(key)), ...f(s.key === key ? s : empty(key)) }));
    const close = openTail(
      q,
      {
        onLog: (l) => pending.current.push(l),
        onDropped: (n) => patch((s) => ({ dropped: s.dropped + n })),
        onOpen: () => patch(() => ({ connected: true })),
        onError: () => patch(() => ({ connected: false })),
      },
      make,
    );
    const id = setInterval(() => {
      if (paused.current) {
        patch(() => ({ waiting: pending.current.length }));
        return;
      }
      if (pending.current.length === 0) return;
      const batch = pending.current;
      pending.current = [];
      patch((s) => ({ logs: mergeTail(s.logs, batch, TAIL_CAP), waiting: 0 }));
    }, TAIL_FLUSH_MS);
    return () => {
      close();
      clearInterval(id);
    };
  }, [enabled, q, key, make]);

  return {
    logs: cur.logs,
    dropped: cur.dropped,
    connected: cur.connected,
    paused: cur.paused,
    waiting: cur.waiting,
    setPaused: (p) => setSnap((s) => ({ ...(s.key === key ? s : empty(key)), paused: p })),
  };
}
