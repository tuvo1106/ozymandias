/**
 * JSON import and export: the definition as a file.
 *
 * Export writes the definition alone — without the `id`, `provisioned` and
 * timestamps a stored dashboard is served with, which the API refuses on the
 * way back in — so an exported file can be committed to
 * `deploy/dashboards/` or posted to another ozyd as it is.
 *
 * Import reads pasted text or a chosen file with [[readImport]] and shows
 * what it found before anything changes: every way of not being a definition
 * is its own message, and replacing the draft is a separate, deliberate
 * click, because it discards whatever is being edited.
 */
import { useState } from "react";
import type { Dashboard } from "../../../lib/dashboard";
import { exportDefinition, readImport } from "../../../lib/dashboardEditor";
import { button } from "./fields";

/** Props for JsonPanel. */
export interface JsonPanelProps {
  dashboard: Dashboard;
  /** Replaces the draft; `dropped` names the database fields removed. */
  onImport: (d: Dashboard, dropped: string[]) => void;
}

/** Export and import of the draft as JSON. */
export function JsonPanel({ dashboard, onImport }: JsonPanelProps) {
  const [text, setText] = useState("");
  const [fileError, setFileError] = useState<string | null>(null);
  const reading = readImport(text);
  const exported = exportDefinition(dashboard);

  const download = () => {
    const url = URL.createObjectURL(new Blob([exported], { type: "application/json" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${dashboard.uid || dashboard.title.replace(/[^\w-]+/g, "-").toLowerCase() || "dashboard"}.json`;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <aside aria-label="JSON" className="flex flex-col gap-4 text-sm">
      <section className="flex flex-col gap-1">
        <div className="flex items-center justify-between">
          <h2 className="font-semibold">Export</h2>
          <button type="button" className={button} onClick={download}>
            Download .json
          </button>
        </div>
        <textarea
          readOnly
          aria-label="Exported definition"
          value={exported}
          rows={10}
          className="rounded-md border border-zinc-300 bg-zinc-50 px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-950"
        />
      </section>

      <section className="flex flex-col gap-1">
        <h2 className="font-semibold">Import</h2>
        <p className="text-xs text-zinc-500">Paste a definition, or choose a file. Nothing changes until you replace the draft.</p>
        <input
          type="file"
          accept="application/json,.json"
          aria-label="Import file"
          onChange={async (e) => {
            const file = e.target.files?.[0];
            if (!file) return;
            try {
              setText(await file.text());
              setFileError(null);
            } catch (err) {
              setFileError(err instanceof Error ? err.message : String(err));
            }
          }}
          className="text-xs"
        />
        {fileError ? (
          <p role="alert" className="text-xs text-red-700 dark:text-red-400">
            Could not read that file: {fileError}
          </p>
        ) : null}
        <textarea
          aria-label="Definition to import"
          value={text}
          onChange={(e) => setText(e.target.value)}
          rows={8}
          className="rounded-md border border-zinc-300 bg-white px-2 py-1 font-mono text-xs dark:border-zinc-700 dark:bg-zinc-900"
        />
        <ImportVerdict reading={reading} />
        <button
          type="button"
          disabled={reading.kind !== "ok"}
          onClick={() => {
            if (reading.kind !== "ok") return;
            onImport(reading.dashboard, reading.dropped);
            setText("");
          }}
          className={`${button} self-start disabled:opacity-40`}
        >
          Replace the draft
        </button>
      </section>
    </aside>
  );
}

function ImportVerdict({ reading }: { reading: ReturnType<typeof readImport> }) {
  switch (reading.kind) {
    case "empty":
      return null;
    case "notJson":
      return (
        <p role="status" className="text-xs text-red-700 dark:text-red-400">
          Not JSON: {reading.message}
        </p>
      );
    case "notDashboard":
      return (
        <div role="status" className="text-xs text-red-700 dark:text-red-400">
          <p>JSON, but not a dashboard definition:</p>
          <ul className="list-disc pl-5">
            {reading.problems.map((p) => (
              <li key={p}>{p}</li>
            ))}
          </ul>
        </div>
      );
    case "ok":
      return (
        <p role="status" className="text-xs text-zinc-600 dark:text-zinc-300">
          {`"${reading.dashboard.title}", ${reading.dashboard.widgets.length} widget${reading.dashboard.widgets.length === 1 ? "" : "s"}.`}
          {reading.dropped.length
            ? ` Its ${reading.dropped.join(", ")} will be left out: those belong to the database a dashboard is stored in, not to the definition.`
            : ""}{" "}
          The server checks the rest when you save.
        </p>
      );
  }
}
