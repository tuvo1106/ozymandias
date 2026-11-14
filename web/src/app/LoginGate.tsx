import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { fetchSession, login, UNAUTHORIZED_EVENT } from "../lib/authApi";

type State = "open" | "login";

/**
 * Puts a password form in front of the app when ozyd asks for one.
 *
 * It renders the app straight away and only swaps to the form on evidence: the
 * session probe says this browser is not logged in, or an API call came back
 * 401. The other order (hold everything until the probe answers) would blank the
 * app for every user of an ozyd with auth off, which is the default, and for the
 * length of a probe that never returns.
 */
export function LoginGate({ children }: { children: ReactNode }) {
  const [state, setState] = useState<State>("open");

  useEffect(() => {
    let live = true;
    void fetchSession().then((s) => {
      if (live && s?.enabled && !s.authenticated) setState("login");
    });
    const onUnauthorized = () => setState("login");
    window.addEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    return () => {
      live = false;
      window.removeEventListener(UNAUTHORIZED_EVENT, onUnauthorized);
    };
  }, []);

  if (state === "login") return <LoginForm onDone={() => window.location.reload()} />;
  return <>{children}</>;
}

function LoginForm({ onDone }: { onDone: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    const err = await login(password);
    setBusy(false);
    if (err === null) onDone();
    else setError(err);
  }

  return (
    <main className="mx-auto mt-24 max-w-sm p-4">
      <h1 className="text-xl font-semibold">Log in to ozymandias</h1>
      <form onSubmit={(e) => void submit(e)} className="mt-4 flex flex-col gap-3">
        <label className="flex flex-col gap-1 text-sm">
          Admin password
          <input
            type="password"
            autoComplete="current-password"
            autoFocus
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className="rounded border border-zinc-300 bg-transparent px-2 py-1 dark:border-zinc-700"
          />
        </label>
        {error && (
          <p role="alert" className="text-sm text-red-600 dark:text-red-400">
            {error}
          </p>
        )}
        <button
          type="submit"
          disabled={busy || password === ""}
          className="rounded bg-violet-600 px-3 py-1.5 text-white disabled:opacity-50"
        >
          Log in
        </button>
      </form>
    </main>
  );
}
