/**
 * The two routes that put a dashboard on screen: one stored by id, and one
 * template instantiated for a service.
 *
 * They share [[DashboardView]] and differ only in where the definition comes
 * from, which is the whole point of instantiating on the server — the UI draws
 * a definition and does not care that this one was never stored.
 */
import { useParams } from "react-router";
import { useDashboard, useServiceDashboards } from "../../lib/useDashboards";
import { DashboardView } from "./DashboardView";

function Loading() {
  return <p className="text-sm text-zinc-500">Loading…</p>;
}

function Failed({ error }: { error: Error }) {
  return (
    <p role="alert" className="rounded-md border border-red-200 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400">
      {error.message}
    </p>
  );
}

/** One stored dashboard, by the id in the path. */
export function StoredDashboardPage() {
  const { id } = useParams();
  const numeric = Number(id);
  const query = useDashboard(Number.isInteger(numeric) && numeric > 0 ? numeric : undefined);

  if (!Number.isInteger(numeric) || numeric <= 0) return <Failed error={new Error(`"${id}" is not a dashboard id`)} />;
  if (query.isPending) return <Loading />;
  if (query.error) return <Failed error={query.error as Error} />;
  if (!query.data) return <Loading />;
  return (
    <DashboardView
      dashboard={query.data}
      syncKey={`dashboard-${query.data.id}`}
      actions={
        query.data.provisioned ? (
          <span className="rounded bg-zinc-100 px-2 py-1 text-xs text-zinc-500 dark:bg-zinc-800">
            Provisioned from a file — edit the file, not this page
          </span>
        ) : null
      }
    />
  );
}

/**
 * Every template instantiated for one service.
 *
 * A list, because nothing says a deployment has one template. They are stacked
 * rather than tabbed: two service overviews are meant to be read together, and
 * a tab hides half of what the page is for.
 */
export function ServiceDashboardPage() {
  const { name = "" } = useParams();
  const query = useServiceDashboards(name);

  if (query.isPending) return <Loading />;
  if (query.error) return <Failed error={query.error as Error} />;
  if (!query.data?.length) {
    return <p className="text-sm text-zinc-500">No template dashboard covers {name}.</p>;
  }
  return (
    <div className="flex flex-col gap-10">
      {query.data.map((instance) => (
        <DashboardView
          key={instance.template_id}
          dashboard={instance.dashboard}
          syncKey={`service-${name}-${instance.template_id}`}
          actions={
            <span className="text-xs text-zinc-500">
              from the {instance.template_uid ?? `#${instance.template_id}`} template
            </span>
          }
        />
      ))}
    </div>
  );
}
