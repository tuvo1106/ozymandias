import { useEffect, useState } from "react";
import { fetchHealth, type Health } from "./health";

/** State of the health poll: loading, a result, or an error message. */
export type HealthState =
  | { kind: "loading" }
  | { kind: "ok"; health: Health }
  | { kind: "error"; message: string };

/**
 * Polls ozyd's /healthz every `intervalMs`, aborting any in-flight
 * request when the component unmounts or the interval changes.
 */
export function useHealth(intervalMs = 10_000, fetchImpl: typeof fetch = fetch): HealthState {
  const [state, setState] = useState<HealthState>({ kind: "loading" });
  useEffect(() => {
    const ctrl = new AbortController();
    const poll = () =>
      fetchHealth(fetchImpl, ctrl.signal).then(
        (health) => setState({ kind: "ok", health }),
        (err: unknown) => {
          if (ctrl.signal.aborted) return;
          setState({ kind: "error", message: err instanceof Error ? err.message : String(err) });
        },
      );
    void poll();
    const id = setInterval(poll, intervalMs);
    return () => {
      ctrl.abort();
      clearInterval(id);
    };
  }, [intervalMs, fetchImpl]);
  return state;
}
