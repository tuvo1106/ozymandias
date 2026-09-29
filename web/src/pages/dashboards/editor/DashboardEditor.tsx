/**
 * The dashboard editor: a draft on a live grid, a panel for whatever is
 * selected, and a save that reports each way it can go.
 *
 * Two routes lead here. `/dashboards/{id}/edit` edits a stored dashboard;
 * `/dashboards/new` starts one — blank, or seeded from a copy of a stored
 * dashboard (`?copy={id}`) or of a template instantiated for a service
 * (`?service={name}&template={template_id}`). The seed is in the URL rather
 * than handed over in router state because router state is gone on reload,
 * and a reload that quietly turned "a copy of Checkout" into a blank page
 * would be the editor concluding "no seed" from "seed not delivered".
 *
 * Either route also takes `?add={query}`: the Metrics Explorer's "Save to
 * dashboard" (ADR-0022). The draft opens with a timeseries widget charting
 * that query added and selected, *unsaved* — the author sees where it lands
 * and saves it through the same save, with the same answers, as any other
 * edit. On `/edit` the parameter is removed once read, or reloading after the
 * save would add the widget a second time; on `/new` it stays, because
 * there it is the seed and nothing has been saved to add it to twice.
 *
 * **The preview is the real page.** It draws with the same grid, the same
 * batch and the same widgets as the view, from a copy of the draft that
 * follows it once typing pauses — so what the editor shows is what the saved
 * dashboard will show. The pause is visible: a widget whose queries have
 * changed since the preview last asked is dimmed and says it is updating,
 * and its answers are matched to their query text ([[widgetResults]]), so a
 * previous answer is never drawn as the current query's.
 */
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useReducer, useState } from "react";
import { Link, useLocation, useNavigate, useParams, useSearchParams } from "react-router";
import type { Dashboard, WidgetType } from "../../../lib/dashboard";
import {
  canonical,
  copyOf,
  definitionOf,
  editDashboard,
  TYPE_RULES,
  WIDGET_TYPES,
  withQueryWidget,
} from "../../../lib/dashboardEditor";
import { collectRequests, sharedWarnings } from "../../../lib/dashboardQueries";
import { problemsOf, saveFailure, type SaveState } from "../../../lib/dashboardSave";
import { createDashboard, updateDashboard } from "../../../lib/dashboardsApi";
import { parseViewState, withViewState, type DashboardViewState } from "../../../lib/dashboardState";
import { useDashboard, useDashboardData, useServiceDashboards } from "../../../lib/useDashboards";
import { useDebouncedValue } from "../../../lib/useDebouncedValue";
import { DashboardGrid } from "../DashboardGrid";
import { DashboardControls, SharedWarnings } from "../DashboardView";
import { DashboardSettings } from "./DashboardSettings";
import { button } from "./fields";
import { JsonPanel } from "./JsonPanel";
import { WidgetEditor } from "./WidgetEditor";

/** The query-key kinds under "dashboards" that hold the preview's data, not definitions. */
const DATA_KEYS = new Set(["batch", "sketches"]);

/**
 * Whether a save makes this cached query stale: everything under
 * "dashboards" but the preview's batch and sketch entries, whose answers a
 * save does not change. Excluded rather than listed, so a definition query
 * added later is invalidated by default — forgetting it would show the old
 * definition, while forgetting a data key only costs a refetch.
 */
export function staleAfterSave(queryKey: readonly unknown[]): boolean {
  return queryKey[0] === "dashboards" && !DATA_KEYS.has(String(queryKey[1]));
}

/** How long the draft must be still before the preview asks about it. */
export const PREVIEW_DEBOUNCE_MS = 400;

function Loading({ what }: { what: string }) {
  return <p className="text-sm text-zinc-500">Loading {what}…</p>;
}

function Failed({ message }: { message: string }) {
  return (
    <p role="alert" className="rounded-md border border-red-200 p-3 text-sm text-red-700 dark:border-red-900 dark:text-red-400">
      {message}
    </p>
  );
}

/** What the Editor opens on: the draft, what counts as unsaved, and the panel. */
interface Opening {
  initial: Dashboard;
  baseline: Dashboard;
  panel: Panel;
}

/** `base`, with the widget `?add=` asks for when it asks for one. */
function opening(base: Dashboard, add: string): Opening {
  if (!add) return { initial: base, baseline: base, panel: { kind: "dashboard" } };
  const { dashboard, id } = withQueryWidget(base, add);
  return { initial: dashboard, baseline: base, panel: { kind: "widget", id } };
}

/** `?add=`, trimmed; "" when absent or blank. */
function addParam(search: string): string {
  return (new URLSearchParams(search).get("add") ?? "").trim();
}

/** `/dashboards/{id}/edit`: a stored dashboard, if it can be edited here. */
export function EditDashboardPage() {
  const { id } = useParams();
  const numeric = Number(id);
  const valid = Number.isInteger(numeric) && numeric > 0;
  const query = useDashboard(valid ? numeric : undefined);
  const location = useLocation();
  const justSaved = (location.state as { saved?: boolean } | null)?.saved === true;
  const add = addParam(location.search);
  // What this arrival carried, remembered per id: the effect below clears
  // both before the dashboard has loaded, and they are for when it has. Per
  // id and not once per mount, because the router keeps this page mounted
  // when only `:id` changes — which is what "save it as a new dashboard" does.
  const [arrival, setArrival] = useState({ id, saved: justSaved, add });
  if (arrival.id !== id || (justSaved && !arrival.saved) || (add && add !== arrival.add))
    setArrival({ id, saved: justSaved, add });
  const navigate = useNavigate();
  // Both are for the render they arrived with, and history keeps them across
  // reloads — so they are cleared once read, or a reload that discarded
  // unsaved edits would open under a "Saved." banner, and one after saving
  // an added widget would add it again.
  useEffect(() => {
    if (!justSaved && !add) return;
    const search = new URLSearchParams(location.search);
    search.delete("add");
    const rest = search.toString();
    navigate({ pathname: location.pathname, search: rest ? `?${rest}` : "", hash: location.hash }, { replace: true, state: null });
  }, [justSaved, add, location, navigate]);
  if (!valid) return <Failed message={`"${id}" is not a dashboard id`} />;
  // Data before error: TanStack keeps `data` and sets `error` when a
  // *refetch* fails, and a failed refetch — ozyd gone for a moment, the
  // invalidation after a save — must not unmount the editor and its draft.
  if (!query.data) {
    if (query.error) return <Failed message={(query.error as Error).message} />;
    return <Loading what="the dashboard" />;
  }
  if (query.data.provisioned) {
    // The server would answer a save with 409; saying so before any editing
    // is kinder than after it.
    return (
      <div className="flex flex-col gap-3">
        <h1 className="text-2xl font-semibold">{query.data.title}</h1>
        <p className="rounded-md border border-zinc-200 p-3 text-sm dark:border-zinc-800">
          This dashboard is provisioned from a file, so an edit saved here would be undone at the next restart. Edit the file — or{" "}
          <Link
            className="underline"
            to={`/dashboards/new?${new URLSearchParams({ copy: String(query.data.id), ...(arrival.add ? { add: arrival.add } : {}) })}`}
          >
            save a copy
          </Link>{" "}
          and edit that.
        </p>
      </div>
    );
  }
  return (
    <Editor
      key={query.data.id}
      {...opening(definitionOf(query.data).definition, arrival.add)}
      storedId={query.data.id}
      initialSave={arrival.saved ? { kind: "saved", dashboard: query.data } : { kind: "idle" }}
    />
  );
}

const BLANK: Dashboard = { title: "Untitled dashboard", widgets: [] };

/** `/dashboards/new`: blank, or a copy of what the query string names. */
export function NewDashboardPage() {
  const [params] = useSearchParams();
  const copyParam = params.get("copy");
  const service = params.get("service") ?? "";
  const template = params.get("template");
  const add = addParam(params.toString());
  const copyId = copyParam === null ? undefined : Number(copyParam);
  const copyValid = copyId !== undefined && Number.isInteger(copyId) && copyId > 0;
  const stored = useDashboard(copyValid ? copyId : undefined);
  const instances = useServiceDashboards(service);

  if (copyParam !== null) {
    if (!copyValid) return <Failed message={`"${copyParam}" is not a dashboard id to copy`} />;
    if (!stored.data) {
      if (stored.error) return <Failed message={`Could not load the dashboard to copy: ${(stored.error as Error).message}`} />;
      return <Loading what="the dashboard to copy" />;
    }
    return <Editor key={`copy-${copyId}-${add}`} {...opening(copyOf(stored.data), add)} initialSave={{ kind: "idle" }} />;
  }
  if (service !== "") {
    if (!instances.data) {
      if (instances.error) return <Failed message={`Could not load ${service}'s dashboards: ${(instances.error as Error).message}`} />;
      return <Loading what={`${service}'s dashboards`} />;
    }
    const instance = instances.data.find((i) => String(i.template_id) === template);
    if (!instance)
      return <Failed message={`No template${template ? ` #${template}` : ""} covers ${service}, so there is nothing to copy.`} />;
    return (
      <Editor
        key={`svc-${service}-${template}-${add}`}
        {...opening(copyOf(instance.dashboard, service), add)}
        initialSave={{ kind: "idle" }}
      />
    );
  }
  return <Editor key={`blank-${add}`} {...opening(BLANK, add)} initialSave={{ kind: "idle" }} />;
}

type Panel = { kind: "dashboard" } | { kind: "widget"; id: string } | { kind: "json" };

/** Props for Editor. */
interface EditorProps extends Opening {
  /** The stored row this edits; undefined for a dashboard not yet saved. */
  storedId?: number;
  initialSave: SaveState;
}

function Editor({ initial, baseline: initialBaseline, panel: initialPanel, storedId: initialId, initialSave }: EditorProps) {
  const [draft, dispatch] = useReducer(editDashboard, initial);
  const [baseline, setBaseline] = useState(initialBaseline);
  const storedId = initialId;
  const [save, setSave] = useState<SaveState>(initialSave);
  const [panel, setPanel] = useState<Panel>(initialPanel);
  const [notice, setNotice] = useState<string | null>(null);
  const [addType, setAddType] = useState<WidgetType>("timeseries");
  const navigate = useNavigate();
  const client = useQueryClient();
  // Memoised per side: the editor re-renders on every preview answer and
  // every pointer move of a drag, and neither changes these.
  const baselineText = useMemo(() => canonical(baseline), [baseline]);
  const draftText = useMemo(() => canonical(draft), [draft]);
  const dirty = draftText !== baselineText;

  // Leaving with unsaved changes asks first. Only the browser's own prompt:
  // an in-app navigation is a link the author chose to click.
  useEffect(() => {
    if (!dirty) return;
    const warn = (e: BeforeUnloadEvent) => e.preventDefault();
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, [dirty]);

  // The preview: a still copy of the draft, scoped by the URL like the view.
  const preview = useDebouncedValue(draft, PREVIEW_DEBOUNCE_MS);
  const [params, setParams] = useSearchParams();
  const state = useMemo(() => parseViewState(params), [params]);
  // The view state is written *beside* the page's own parameters: on
  // /dashboards/new the query string also holds the seed (?copy=, ?service=,
  // ?template=), and replacing it wholesale would turn the page into a blank
  // new dashboard — remounting the editor and discarding the draft.
  const update = (patch: Partial<DashboardViewState>) =>
    setParams(withViewState(params, { ...state, ...patch }), { replace: true });
  const requests = useMemo(() => collectRequests(preview.widgets), [preview.widgets]);
  const syncKey = `editor-${storedId ?? "new"}`;
  const data = useDashboardData(requests, state, preview.template_vars, syncKey);
  const stale = useMemo(() => {
    const asked = new Map(preview.widgets.map((w) => [w.id, w]));
    return new Set(
      draft.widgets
        .filter((w) => {
          const p = asked.get(w.id);
          return !p || p.type !== w.type || JSON.stringify(p.queries) !== JSON.stringify(w.queries);
        })
        .map((w) => w.id),
    );
  }, [draft.widgets, preview.widgets]);
  const shared = useMemo(() => sharedWarnings(draft.widgets, data.byWidget, data.sketches), [draft.widgets, data.byWidget, data.sketches]);
  const hidden = useMemo(() => new Set(shared), [shared]);

  const selected = panel.kind === "widget" ? draft.widgets.find((w) => w.id === panel.id) : undefined;
  // Saving again after a create whose answer could not be read would create
  // a second dashboard: the first exists, and this page does not know its id.
  const locked = save.kind === "savedUnreadable" && save.created;

  const doSave = async (asNew: boolean) => {
    const sent = definitionOf(draft).definition;
    const id = asNew ? undefined : storedId;
    setSave({ kind: "saving" });
    try {
      const r = id === undefined ? await createDashboard(sent) : await updateDashboard(id, sent);
      setBaseline(draft);
      // Not awaited: the save is done when ozyd said so.
      void client.invalidateQueries({ predicate: (q) => staleAfterSave(q.queryKey) });
      if (r.kind === "unreadable") {
        setSave({ kind: "savedUnreadable", created: id === undefined });
        return;
      }
      setSave({ kind: "saved", dashboard: r.dashboard });
      if (id === undefined) navigate(`/dashboards/${r.dashboard.id}/edit`, { replace: true, state: { saved: true } });
    } catch (e) {
      setSave(saveFailure(e, id === undefined));
    }
  };

  return (
    <div className="flex flex-col gap-4">
      <header className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-2xl font-semibold">{draft.title || "Untitled"}</h1>
          <p className="text-xs text-zinc-500">
            {storedId === undefined ? "New dashboard, not saved yet" : `Editing dashboard #${storedId}`}
            {dirty ? " · unsaved changes" : ""}
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <label className="flex items-center gap-1">
            <span className="sr-only">Widget type to add</span>
            <select
              aria-label="Widget type to add"
              value={addType}
              onChange={(e) => setAddType(e.target.value as WidgetType)}
              className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
            >
              {WIDGET_TYPES.map((t) => (
                <option key={t} value={t}>
                  {TYPE_RULES[t].label}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            className={button}
            onClick={() => {
              const next = editDashboard(draft, { type: "addWidget", widgetType: addType });
              dispatch({ type: "replace", dashboard: next });
              const added = next.widgets[next.widgets.length - 1];
              if (added) setPanel({ kind: "widget", id: added.id });
            }}
          >
            Add widget
          </button>
          <button type="button" className={button} onClick={() => setPanel({ kind: "dashboard" })}>
            Dashboard settings
          </button>
          <button type="button" className={button} onClick={() => setPanel({ kind: "json" })}>
            JSON
          </button>
          {storedId !== undefined ? (
            <Link className={button} to={`/dashboards/${storedId}`}>
              {dirty ? "Leave without saving" : "Done"}
            </Link>
          ) : null}
          <button
            type="button"
            disabled={save.kind === "saving" || locked || (!dirty && storedId !== undefined)}
            onClick={() => void doSave(false)}
            className="rounded-md bg-violet-600 px-3 py-1 text-sm font-medium text-white hover:bg-violet-700 disabled:opacity-40"
          >
            {save.kind === "saving" ? "Saving…" : storedId === undefined ? "Create" : "Save"}
          </button>
        </div>
      </header>

      <SaveStatus save={save} dirty={dirty} onSaveAsNew={() => void doSave(true)} />
      {notice ? (
        <p role="status" className="text-xs text-zinc-600 dark:text-zinc-300">
          {notice}
        </p>
      ) : null}

      <DashboardControls dashboard={preview} state={state} onChange={update} />

      <div className="flex flex-col gap-4 lg:flex-row">
        <div className="min-w-0 flex-1">
          {data.error ? <Failed message={data.error.message} /> : null}
          {draft.widgets.length === 0 ? (
            <p className="rounded-lg border border-dashed border-zinc-300 p-8 text-center text-sm text-zinc-500 dark:border-zinc-700">
              No widgets yet. Choose a type and add one; a dashboard needs at least one to be saved.
            </p>
          ) : (
            <div className={data.isRefreshing ? "opacity-50 transition-opacity" : "transition-opacity"}>
              <SharedWarnings warnings={shared} />
              <DashboardGrid
                widgets={draft.widgets}
                byWidget={data.byWidget}
                sketches={data.sketches}
                xRange={data.range ? [data.range.from, data.range.to] : undefined}
                syncKey={syncKey}
                hiddenWarnings={hidden}
                edit={{
                  selected: selected?.id,
                  onSelect: (id) => setPanel({ kind: "widget", id }),
                  onLayout: (id, layout, settle) => dispatch({ type: "setLayout", id, layout, settle }),
                  stale,
                }}
              />
            </div>
          )}
        </div>
        <div className="w-full shrink-0 rounded-lg border border-zinc-200 p-3 lg:w-[26rem] dark:border-zinc-800">
          {panel.kind === "json" ? (
            <JsonPanel
              dashboard={draft}
              onImport={(d, dropped) => {
                dispatch({ type: "replace", dashboard: d });
                setPanel({ kind: "dashboard" });
                setNotice(
                  `Imported "${d.title}".${dropped.length ? ` Left out ${dropped.join(", ")}, which belong to the database, not the definition.` : ""} Save to keep it.`,
                );
              }}
            />
          ) : selected ? (
            <WidgetEditor
              key={selected.id}
              widget={selected}
              dashboard={draft}
              dispatch={dispatch}
              onClose={() => setPanel({ kind: "dashboard" })}
            />
          ) : (
            // A selected widget that no longer exists (removed, or replaced by
            // an import) falls back to the dashboard's own settings.
            <DashboardSettings dashboard={draft} dispatch={dispatch} />
          )}
        </div>
      </div>
    </div>
  );
}

/**
 * What the last save did. One branch per [[SaveState]]; no default, so a new
 * state fails to compile here rather than rendering nothing.
 */
function SaveStatus({
  save,
  dirty,
  onSaveAsNew,
}: {
  save: SaveState;
  dirty: boolean;
  onSaveAsNew: () => void;
}) {
  const box = (tone: string, children: React.ReactNode, role: "status" | "alert" = "status") => (
    <div role={role} className={`rounded-md border p-3 text-sm ${tone}`}>
      {children}
    </div>
  );
  const bad = "border-red-200 text-red-700 dark:border-red-900 dark:text-red-400";
  switch (save.kind) {
    case "idle":
    case "saving":
      return null;
    case "saved":
      return dirty
        ? null
        : box(
            "border-emerald-200 text-emerald-800 dark:border-emerald-900 dark:text-emerald-400",
            <>
              Saved.{" "}
              <Link className="underline" to={`/dashboards/${save.dashboard.id}`}>
                View the dashboard
              </Link>
            </>,
          );
    case "savedUnreadable":
      return box(
        "border-amber-200 text-amber-800 dark:border-amber-900 dark:text-amber-400",
        save.created ? (
          <>
            Saved — ozyd said so — but its answer could not be read, so this page does not know the new dashboard&apos;s id. Find it in the{" "}
            <Link className="underline" to="/dashboards">
              list
            </Link>
            ; saving again from here would create a second copy.
          </>
        ) : (
          "Saved — ozyd said so — but its answer could not be read. Reload to see what was stored."
        ),
      );
    case "refused":
      return box(
        bad,
        <>
          <p>ozyd refused this definition:</p>
          <ul className="mt-1 list-disc pl-5">
            {problemsOf(save.message).map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </>,
        "alert",
      );
    case "conflict":
      return box(bad, <p>Not saved: {save.message}</p>, "alert");
    case "gone":
      return box(
        bad,
        <>
          This dashboard was deleted while you were editing it. Your draft is still here.{" "}
          <button type="button" className="underline" onClick={onSaveAsNew}>
            Save it as a new dashboard
          </button>
        </>,
        "alert",
      );
    case "unreachable":
      // Not "nothing was written": a request can reach the server and lose
      // its answer on the way back, and from here the two look the same.
      return box(
        bad,
        <p>
          {save.message}. Whether this was saved is not known{save.created ? " — check the dashboard list before creating it again, or it may exist twice" : "; saving again is safe"}.
        </p>,
        "alert",
      );
    case "failed":
      return box(bad, <p>Not saved: {save.status ? `ozyd answered ${save.status}: ` : ""}{save.message}</p>, "alert");
  }
}
