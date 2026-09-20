import { Link } from "react-router";
import { formatUptime } from "../lib/health";
import { NAV_ITEMS } from "../lib/nav";
import { useHealth } from "../lib/useHealth";

/**
 * Landing page. Until the Overview dashboard exists (M3), it shows whether
 * the UI can reach ozyd and what each section of the product will do.
 */
export function Home() {
  const health = useHealth();
  return (
    <div className="max-w-3xl">
      <h1 className="text-2xl font-semibold">ozymandias</h1>
      <p className="mt-1 text-zinc-600 dark:text-zinc-400">
        An observability platform, built from scratch to learn how these systems work.
      </p>

      <section aria-label="Server status" className="mt-6 rounded-lg border border-zinc-200 p-4 dark:border-zinc-800">
        {health.kind === "loading" && <p className="text-zinc-500">Checking ozyd…</p>}
        {health.kind === "error" && (
          <p role="alert" className="text-red-600 dark:text-red-400">
            {health.message}
          </p>
        )}
        {health.kind === "ok" && (
          <p>
            <span className="mr-2 inline-block h-2 w-2 rounded-full bg-emerald-500" aria-hidden />
            {health.health.component} {health.health.version} is up — {formatUptime(health.health.uptime_seconds)}
          </p>
        )}
      </section>

      <h2 className="mt-8 text-lg font-semibold">Sections</h2>
      <ul className="mt-3 grid gap-3 sm:grid-cols-2">
        {NAV_ITEMS.map((item) => (
          <li key={item.path}>
            <Link
              to={item.path}
              className="block rounded-lg border border-zinc-200 p-4 hover:border-violet-400 dark:border-zinc-800"
            >
              <span className="font-medium">{item.label}</span>
              <span className={`ml-2 text-xs ${item.live ? "text-emerald-600 dark:text-emerald-400" : "text-zinc-500"}`}>
                {item.live ? "live" : item.milestone}
              </span>
              <p className="mt-1 text-sm text-zinc-600 dark:text-zinc-400">{item.description}</p>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}
