/**
 * Client for ozyd's `GET /healthz` (docs/api.md), plus the formatting the
 * UI needs to show it. Pure functions take their dependencies (fetch) as
 * arguments so they are testable without a server.
 */

/** The /healthz response body. */
export interface Health {
  status: string;
  component: string;
  version: string;
  uptime_seconds: number;
}

/**
 * Fetches and validates /healthz. Throws with a readable message on network
 * failure, a non-2xx status or a body that isn't a health document — the
 * caller shows that message as-is.
 */
export async function fetchHealth(fetchImpl: typeof fetch = fetch, signal?: AbortSignal): Promise<Health> {
  let res: Response;
  try {
    res = await fetchImpl("/healthz", { signal, headers: { Accept: "application/json" } });
  } catch (err) {
    if (err instanceof DOMException && err.name === "AbortError") throw err;
    throw new Error(`ozyd is unreachable: ${err instanceof Error ? err.message : String(err)}`, { cause: err });
  }
  if (!res.ok) {
    throw new Error(`ozyd answered ${res.status} ${res.statusText}`.trim());
  }
  const body: unknown = await res.json().catch(() => undefined);
  if (!isHealth(body)) {
    throw new Error("ozyd sent an unexpected /healthz response");
  }
  return body;
}

function isHealth(v: unknown): v is Health {
  if (typeof v !== "object" || v === null) return false;
  const h = v as Record<string, unknown>;
  return (
    typeof h.status === "string" &&
    typeof h.component === "string" &&
    typeof h.version === "string" &&
    typeof h.uptime_seconds === "number"
  );
}

/**
 * Formats a duration in seconds as its two most significant units:
 * "45s", "3m 20s", "5h 2m", "12d 4h". Negative or non-finite input is "—".
 */
export function formatUptime(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  const s = Math.floor(seconds);
  const units: [string, number][] = [
    ["d", Math.floor(s / 86400)],
    ["h", Math.floor(s / 3600) % 24],
    ["m", Math.floor(s / 60) % 60],
    ["s", s % 60],
  ];
  const first = units.findIndex(([, n]) => n > 0);
  if (first === -1) return "0s";
  return units
    .slice(first, first + 2)
    .filter(([, n], i) => i === 0 || n > 0)
    .map(([u, n]) => `${n}${u}`)
    .join(" ");
}
