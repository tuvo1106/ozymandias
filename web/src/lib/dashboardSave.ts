/**
 * What the editor says about a save, as one value per way it can go.
 *
 * The status codes are the API's (api.md, "Status codes"), and each is a
 * different instruction to the author:
 *
 *   - **400** — the definition does not validate. The message names every
 *     problem, so it is shown whole rather than summarised.
 *   - **409** — a conflict, which is *two* things on this API: the dashboard
 *     is provisioned from a file, or the `uid` belongs to another dashboard.
 *     The server's sentence says which, so it is shown as-is rather than
 *     guessed at from the code.
 *   - **404** — the dashboard was deleted while it was being edited. The
 *     draft is still here; saving it as new is the way out.
 *   - **no status** — ozyd could not be reached. Nothing is known about
 *     whether anything was written — but a request that never connected wrote
 *     nothing, so trying again is safe.
 *   - anything else — ozyd's own failure, with its status.
 */
import { ApiError } from "./metricsApi";
import type { StoredDashboard } from "./dashboard";

/** The editor's save state. */
export type SaveState =
  | { kind: "idle" }
  | { kind: "saving" }
  | { kind: "saved"; dashboard: StoredDashboard }
  /** Saved — the server said so — but its answer could not be read. */
  | { kind: "savedUnreadable" }
  | { kind: "refused"; message: string }
  | { kind: "conflict"; message: string }
  | { kind: "gone" }
  | { kind: "unreachable"; message: string }
  | { kind: "failed"; status: number | undefined; message: string };

/** The state a failed save leaves the editor in. */
export function saveFailure(e: unknown): SaveState {
  if (!(e instanceof ApiError)) {
    return { kind: "failed", status: undefined, message: e instanceof Error ? e.message : String(e) };
  }
  switch (e.status) {
    case 400:
      return { kind: "refused", message: e.message };
    case 409:
      return { kind: "conflict", message: e.message };
    case 404:
      return { kind: "gone" };
    case undefined:
      return { kind: "unreachable", message: e.message };
    default:
      return { kind: "failed", status: e.status, message: e.message };
  }
}

/**
 * The server's 400 message as a list. A definition's problems arrive joined
 * by newlines (Go's `errors.Join`), each prefixed with the sentinel "invalid
 * dashboard: "; a list is what the author reads them as.
 */
export function problemsOf(message: string): string[] {
  return message
    .split("\n")
    .map((l) => l.replace(/^invalid dashboard:\s*/, "").trim())
    .filter((l) => l !== "");
}
