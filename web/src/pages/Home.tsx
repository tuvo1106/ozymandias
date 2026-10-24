import { lazy, Suspense } from "react";
import { Link } from "react-router";
import { formatUptime } from "../lib/health";
import { NAV_ITEMS } from "../lib/nav";
import { useDashboardList } from "../lib/useDashboards";
import { useHealth } from "../lib/useHealth";

// Lazy: the dashboard pulls in the chart library, which reads matchMedia as it
// loads; Home is in the first bundle, and the shell should not wait for (or
// break on) a chart library to say whether ozyd is up.
const DashboardView = lazy(async () => ({ default: (await import("./dashboards/DashboardView")).DashboardView }));

/** The uid of the dashboard Home shows, provisioned from deploy/dashboards/home.json. */
export const HOME_UID = "home";

/**
 * Landing page: whether the UI can reach ozyd, then the `home` dashboard, the
 * same widget system as any other (docs/plan/ui.md): per-service health
 * cards, triggered monitors, ingest health.
 *
 * Without a `home` dashboard (not provisioned, or removed) it says so and
 * falls back to the section list, so the first page is never blank.
 */
export function Home() {
  const health = useHealth();
  const list = useDashboardList();
  const home = list.data?.dashboards.find((d) => d.uid === HOME_UID);
  return (
    <div className="flex flex-col gap-4">
      <section aria-label="Server status" className="rounded-lg border border-zinc-200 px-4 py-2 text-sm dark:border-zinc-800">
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

      {home ? (
        <Suspense fallback={<p className="text-sm text-zinc-500">Loading the home dashboard…</p>}>
          <DashboardView dashboard={home} syncKey="home" />
        </Suspense>
      ) : list.isPending ? (
        <p className="text-sm text-zinc-500">Loading the home dashboard…</p>
      ) : (
        <Sections
          note={
            list.error
              ? `The dashboards could not be listed: ${(list.error as Error).message}`
              : `No dashboard has the uid "${HOME_UID}", so there is nothing to show here yet. Provision deploy/dashboards/home.json to get service health, triggered monitors and ingest health on this page.`
          }
        />
      )}
    </div>
  );
}

function Sections({ note }: { note: string }) {
  return (
    <div className="max-w-3xl">
      <h1 className="text-2xl font-semibold">ozymandias</h1>
      <p className="mt-1 text-zinc-600 dark:text-zinc-400">
        An observability platform, built from scratch to learn how these systems work.
      </p>
      <p role="note" className="mt-3 text-sm text-amber-700 dark:text-amber-500">
        {note}
      </p>
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
