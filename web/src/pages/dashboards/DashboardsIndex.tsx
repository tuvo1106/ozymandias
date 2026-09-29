/**
 * The dashboard picker: everything stored, and every service a template covers.
 *
 * Two lists rather than one, because the two are different kinds of thing. A
 * stored dashboard is a document somebody wrote and can edit; a service
 * dashboard is a template instantiated for a name discovered in the data, and
 * there is no row behind it to open. Merging them would mean explaining why
 * half the list cannot be deleted.
 */
import { Link } from "react-router";
import { useDashboardList, useServices } from "../../lib/useDashboards";

function Panel({
  title,
  children,
}: {
  title: string;
  children: React.ReactNode;
}) {
  return (
    <section aria-label={title} className="flex flex-col gap-2">
      <h2 className="text-lg font-medium">{title}</h2>
      {children}
    </section>
  );
}

/** Lists the dashboards and the services with one. */
export function DashboardsIndex() {
  const list = useDashboardList();
  const services = useServices();
  // A template is not shown as itself — it has no data of its own, only the
  // services below. So the empty state counts the rows that are *drawn*: a
  // deployment provisioning nothing but templates has rows and an empty list,
  // and a panel that is blank without saying why is the thing this message is
  // for.
  const saved = list.data?.dashboards.filter((d) => !d.template);

  return (
    <div className="flex flex-col gap-8">
      <div className="flex items-center justify-between gap-2">
        <h1 className="text-2xl font-semibold">Dashboards</h1>
        <Link
          to="/dashboards/new"
          className="rounded-md bg-violet-600 px-3 py-1 text-sm font-medium text-white hover:bg-violet-700"
        >
          New dashboard
        </Link>
      </div>

      <Panel title="Saved">
        {list.isPending ? (
          <p className="text-sm text-zinc-500">Loading…</p>
        ) : null}
        {list.error ? (
          <p role="alert" className="text-sm text-red-700 dark:text-red-400">
            {list.error.message}
          </p>
        ) : null}
        {saved?.length === 0 ? (
          <p className="text-sm text-zinc-500">
            None yet. Dashboards provisioned from{" "}
            <code>provisioning.paths</code> appear here at startup.
          </p>
        ) : null}
        <ul className="flex flex-col gap-1">
          {saved?.map((d) => (
            <li key={d.id}>
              <Link
                to={`/dashboards/${d.id}`}
                className="flex items-baseline gap-2 rounded-md px-2 py-1.5 hover:bg-zinc-100 dark:hover:bg-zinc-800"
              >
                <span className="font-medium">{d.title}</span>
                {d.provisioned ? (
                  <span
                    title="Provisioned from a file; edits through the UI would be undone at the next restart."
                    className="rounded bg-zinc-100 px-1.5 py-0.5 text-xs text-zinc-500 dark:bg-zinc-800"
                  >
                    from file
                  </span>
                ) : null}
                {d.description ? (
                  <span className="truncate text-sm text-zinc-500">
                    {d.description}
                  </span>
                ) : null}
              </Link>
            </li>
          ))}
        </ul>
        {/* A row that exists and cannot be rendered is worth naming: the
            response tells us it is there, and silence would make it look
            deleted. */}
        {list.data?.unreadable.length ? (
          <p className="text-xs text-amber-700 dark:text-amber-500">
            {list.data.unreadable.length} stored dashboard(s) could not be read
            (ids {list.data.unreadable.join(", ")}); ozyd's log says why.
          </p>
        ) : null}
      </Panel>

      <Panel title="Services">
        <p className="text-sm text-zinc-500">
          Every service a template dashboard covers, discovered from the metrics
          the templates query.
        </p>
        {services.error ? (
          <p role="alert" className="text-sm text-red-700 dark:text-red-400">
            {services.error.message}
          </p>
        ) : null}
        {services.data?.services.length === 0 ? (
          <p className="text-sm text-zinc-500">
            No service is reporting a metric any template queries yet.
          </p>
        ) : null}
        <ul className="flex flex-wrap gap-2">
          {services.data?.services.map((name) => (
            <li key={name}>
              <Link
                to={`/dashboards/service/${encodeURIComponent(name)}`}
                className="rounded-md border border-zinc-200 px-2 py-1 text-sm hover:bg-zinc-100 dark:border-zinc-800 dark:hover:bg-zinc-800"
              >
                {name}
              </Link>
            </li>
          ))}
        </ul>
        {services.data?.truncated ? (
          <p className="text-xs text-amber-700 dark:text-amber-500">
            This list is partial: ozyd stopped before covering every service.
          </p>
        ) : null}
      </Panel>
    </div>
  );
}
