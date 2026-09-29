/**
 * The box every widget is drawn in: a title, and whatever the widget could not
 * draw.
 *
 * Every widget has the same three failure modes and they are worth showing the
 * same way each time, because the reader's question is always "is this widget
 * broken or is the system quiet?":
 *
 *   - **an error** — this query was refused, and the message says why. It is
 *     per widget rather than per dashboard on purpose (ADR-0017): one typo in
 *     one widget must not blank the other eleven. It is drawn *beside* the
 *     body and not instead of it, for the same reason one level down: a
 *     two-query chart whose second query has a typo still has a first query
 *     that answered, and throwing that line away is the per-widget version of
 *     the mistake ADR-0017 is about.
 *   - **warnings** — it answered, and something about the answer is worth
 *     knowing (a variable that resolved to nothing, a series cap hit). Shown
 *     beside the data, never instead of it.
 *   - **nothing** — it answered with no series at all, which is a legitimate
 *     picture of a service that is not reporting. Saying "no data" is the
 *     difference between that and a widget that failed silently.
 */
import type { ReactNode } from "react";

/** Props for WidgetFrame. */
export interface WidgetFrameProps {
  title?: string;
  /** A message from the server; renders above the body. */
  error?: string;
  /** Messages from the server; render above the body. */
  warnings?: readonly string[];
  /**
   * True when the widget answered and has nothing to draw. False while the
   * answer is still on its way — "No data" written before anything has arrived
   * tells the reader their service is silent, which is the one thing a
   * dashboard must not say by accident.
   */
  empty?: boolean;
  /**
   * True when the widget has nothing to ask: every query is blank. A fourth
   * state beside error, warnings and empty, because "No data" says the
   * service is silent, and a widget with no query has not asked anybody —
   * which is every new widget in the editor.
   */
  unasked?: boolean;
  children?: ReactNode;
}

/** Draws a widget's chrome, or its error. */
export function WidgetFrame({
  title,
  error,
  warnings,
  empty,
  unasked,
  children,
}: WidgetFrameProps) {
  return (
    <section
      aria-label={title ?? "Widget"}
      className="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border border-zinc-200 bg-white p-3 dark:border-zinc-800 dark:bg-zinc-900"
    >
      {title ? (
        <h3 className="mb-2 shrink-0 truncate text-sm font-medium text-zinc-600 dark:text-zinc-300">
          {title}
        </h3>
      ) : null}
      {warnings?.length ? (
        <ul className="mb-2 shrink-0 space-y-0.5 text-xs text-amber-700 dark:text-amber-500">
          {warnings.map((w) => (
            <li key={w}>{w}</li>
          ))}
        </ul>
      ) : null}
      {error ? (
        <p
          role="status"
          className="mb-2 shrink-0 text-xs text-red-700 dark:text-red-400"
        >
          {error}
        </p>
      ) : null}
      <div className="min-h-0 flex-1">
        {/* Nothing to draw and no error: say so. Nothing to draw and an error:
            the error has already said so, and "No data" under it reads as a
            second, separate problem. */}
        {unasked ? (
          <p role="status" className="text-xs text-zinc-500">
            No query yet
          </p>
        ) : empty ? (
          error ? null : (
            <p role="status" className="text-xs text-zinc-500">
              No data
            </p>
          )
        ) : (
          children
        )}
      </div>
    </section>
  );
}
