/**
 * Login for an ozyd that has auth on (ADR-0059). With auth off the session
 * endpoint does not exist, which is how the UI tells the two apart: absent means
 * "no login needed", not "broken".
 */

/** Fired when an API call is answered 401: the session ended, or never began. */
export const UNAUTHORIZED_EVENT = "ozy:unauthorized";

/** What GET /api/v1/auth/session says. */
export interface Session {
  enabled: boolean;
  authenticated: boolean;
}

/**
 * Asks whether this browser needs to log in. Anything other than a well-formed
 * 200 (a 404 from an ozyd with auth off, a network error, a body that is not a
 * session) is `null`: the UI then shows the app and lets individual calls fail
 * as they would, rather than putting a login form in front of a server that
 * has no login.
 */
export async function fetchSession(): Promise<Session | null> {
  try {
    const res = await fetch("/api/v1/auth/session");
    if (!res.ok) return null;
    const body: unknown = await res.json();
    if (typeof body !== "object" || body === null) return null;
    const b = body as Record<string, unknown>;
    if (typeof b.enabled !== "boolean" || typeof b.authenticated !== "boolean") return null;
    return { enabled: b.enabled, authenticated: b.authenticated };
  } catch {
    return null;
  }
}

/**
 * Asks the server whether the session is still good and, if not, tells the login
 * gate. For failures that carry no status: an EventSource error is just an
 * event, so a tail cut off by an expired session looks like a dropped connection
 * until something asks.
 */
export async function notifyIfLoggedOut(): Promise<void> {
  const s = await fetchSession();
  if (s?.enabled && !s.authenticated) window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
}

/** Logs in; resolves to an error message, or null on success. */
export async function login(password: string): Promise<string | null> {
  let res: Response;
  try {
    res = await fetch("/api/v1/auth/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password }),
    });
  } catch {
    return "ozyd is unreachable";
  }
  if (res.ok) return null;
  if (res.status === 401) return "Wrong password";
  if (res.status === 429) return "Too many attempts; wait a moment";
  return `Login failed (${res.status})`;
}
