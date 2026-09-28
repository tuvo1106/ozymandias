/**
 * The twelve-column grid a dashboard is laid out on.
 *
 * CSS grid rather than absolute positioning: the definition already speaks in
 * cells (`{x, y, w, h}`), which is the same model `grid-column` and `grid-row`
 * use, so the translation is arithmetic rather than layout code. It also means
 * the browser handles what happens when the window is narrower than twelve
 * usable columns, and a dashboard prints.
 *
 * Rows are a fixed pixel height because charts need one: uPlot draws to a
 * canvas of a given size, so "as tall as the content" is not available to us.
 * The grid stops at the cell, though: how much of a cell is left for a canvas
 * depends on whether that widget has a title, a warnings row and a legend, and
 * a constant here that tried to guess clipped the legend off every chart with
 * a warning above it. Each chart measures the box it was given instead.
 */
import { useRef, type KeyboardEvent, type PointerEvent } from "react";
import {
  gridArea,
  gridRows,
  widgetsInReadingOrder,
  type Layout,
  type Widget,
} from "../../lib/dashboard";
import { cellDelta, moveBy, resizeBy } from "../../lib/gridLayout";
import type { BatchResult } from "../../lib/dashboardsApi";
import type { SketchState } from "../../lib/useDashboards";
import { widgetResults, widgetSketch, type Answer } from "../../lib/dashboardQueries";
import { HeatmapWidget } from "./HeatmapWidget";
import { WidgetFrame } from "./WidgetFrame";
import {
  NoteWidget,
  QueryValueWidget,
  TableWidget,
  TimeseriesWidget,
  ToplistWidget,
  type WidgetProps,
} from "./widgets";

/** Height of one grid row in CSS pixels. */
export const ROW_HEIGHT = 64;

/** Props for DashboardGrid. */
export interface DashboardGridProps {
  widgets: readonly Widget[];
  /** Results by widget id, then by the query's index within that widget. */
  byWidget: Map<string, Map<number, Answer<BatchResult>>>;
  /** Sketches by widget id; only heatmaps have one. */
  sketches: Map<string, SketchState>;
  /** The evaluated window, so every chart's x-axis covers it. */
  xRange?: [number, number];
  /** Charts sharing this key share a cursor; one per dashboard. */
  syncKey: string;
  /** Warnings said once above the grid; see [[sharedWarnings]]. */
  hiddenWarnings?: ReadonlySet<string>;
  /** Present in the editor: makes every widget movable and resizable. */
  edit?: GridEditing;
}

/** What the editor hands the grid. */
export interface GridEditing {
  selected: string | undefined;
  onSelect: (id: string) => void;
  /**
   * A new layout for a widget. `settle` is false while a drag is in
   * progress and true when it ends — the moment collisions are resolved,
   * so widgets are not shoved around under a pointer that is still moving.
   */
  onLayout: (id: string, layout: Layout, settle: boolean) => void;
  /**
   * Widgets whose preview is for an older version of their queries than the
   * editor holds — the preview asks after typing pauses, so for a moment
   * the two differ, and the widget says so rather than presenting the old
   * answer as the new one's.
   */
  stale: ReadonlySet<string>;
}

/** The gap between cells, matching the grid's `gap-3`. */
const GAP = 12;

/**
 * A widget this build does not know how to draw.
 *
 * The table below is total over the union, but the union is this bundle's idea
 * of the types — and the client validates `type` only as "a string", because
 * the server is the one that validates definitions. A server newer than the
 * page, or a definition that reached the store some other way, therefore
 * produces a type with no renderer. Without this, `<undefined />` throws during
 * render and React Router's default boundary replaces the whole application,
 * sidebar included, with "Unexpected Application Error" — one unrecognised
 * widget destroying the page that the rest of this file works to keep isolated
 * (ADR-0017).
 */
function UnknownWidget({ widget }: WidgetProps) {
  return (
    <WidgetFrame
      title={widget.title}
      error={`This build cannot draw a "${widget.type}" widget.`}
    />
  );
}

/**
 * One renderer per widget type, as a table rather than a switch: the compiler
 * then requires an entry for every member of the union, so adding a type to
 * the definition fails here instead of rendering a blank square.
 */
const RENDERERS: Record<
  Widget["type"],
  (props: WidgetProps) => React.ReactElement
> = {
  timeseries: TimeseriesWidget,
  query_value: QueryValueWidget,
  toplist: ToplistWidget,
  table: TableWidget,
  heatmap: HeatmapWidget,
  note: NoteWidget,
};

/** Draws every widget in its place. */
export function DashboardGrid({
  widgets,
  byWidget,
  sketches,
  xRange,
  syncKey,
  hiddenWarnings,
  edit,
}: DashboardGridProps) {
  const ordered = widgetsInReadingOrder(widgets);
  const gridRef = useRef<HTMLDivElement>(null);
  return (
    <div
      ref={gridRef}
      data-testid="dashboard-grid"
      className="grid gap-3"
      style={{
        gridTemplateColumns: "repeat(12, minmax(0, 1fr))",
        gridTemplateRows: `repeat(${Math.max(gridRows(widgets), 1)}, ${ROW_HEIGHT}px)`,
      }}
    >
      {ordered.map((widget) => {
        const Renderer = RENDERERS[widget.type] ?? UnknownWidget;
        return (
          <div
            key={widget.id}
            style={gridArea(widget.layout)}
            className={`min-h-0 min-w-0 ${edit ? "relative" : ""} ${
              edit?.selected === widget.id ? "rounded-lg ring-2 ring-violet-500" : ""
            } ${edit?.stale.has(widget.id) ? "opacity-60" : ""}`}
            onClickCapture={edit ? () => edit.onSelect(widget.id) : undefined}
          >
            {edit ? <EditHandles widget={widget} edit={edit} gridRef={gridRef} /> : null}
            <Renderer
              widget={widget}
              results={widgetResults(widget, byWidget)}
              xRange={xRange}
              syncKey={syncKey}
              sketch={widgetSketch(widget, sketches)}
              hiddenWarnings={hiddenWarnings}
            />
          </div>
        );
      })}
    </div>
  );
}

/**
 * The move bar and resize corner the editor puts on a widget.
 *
 * Pointer drags are measured against the grid's own width, so a cell is
 * whatever the browser made it; [[cellDelta]] turns pixels into cells and
 * [[moveBy]] / [[resizeBy]] keep the result on the grid. Both handles are
 * buttons and take the arrow keys, which is what makes the grid usable
 * without a mouse — and testable without layout.
 */
function EditHandles({
  widget,
  edit,
  gridRef,
}: {
  widget: Widget;
  edit: GridEditing;
  gridRef: React.RefObject<HTMLDivElement | null>;
}) {
  const drag = useRef<{
    mode: "move" | "resize";
    x: number;
    y: number;
    start: Layout;
    last: Layout;
  } | null>(null);
  const name = widget.title ?? widget.id;

  const down = (mode: "move" | "resize", e: PointerEvent<HTMLButtonElement>) => {
    e.currentTarget.setPointerCapture?.(e.pointerId);
    drag.current = { mode, x: e.clientX, y: e.clientY, start: widget.layout, last: widget.layout };
    edit.onSelect(widget.id);
  };
  const move = (e: PointerEvent<HTMLButtonElement>) => {
    const d = drag.current;
    const grid = gridRef.current;
    if (!d || !grid) return;
    const { dx, dy } = cellDelta(e.clientX - d.x, e.clientY - d.y, {
      width: grid.getBoundingClientRect().width,
      gap: GAP,
      rowHeight: ROW_HEIGHT,
    });
    const next = d.mode === "move" ? moveBy(d.start, dx, dy) : resizeBy(d.start, dx, dy);
    if (next.x === d.last.x && next.y === d.last.y && next.w === d.last.w && next.h === d.last.h) return;
    d.last = next;
    edit.onLayout(widget.id, next, false);
  };
  const up = () => {
    const d = drag.current;
    drag.current = null;
    if (d) edit.onLayout(widget.id, d.last, true);
  };
  // A cancelled drag (the pointer left the window, a touch was taken over)
  // puts the widget back where it started rather than where it happened to be.
  const cancel = () => {
    const d = drag.current;
    drag.current = null;
    if (d) edit.onLayout(widget.id, d.start, true);
  };
  const keys = (mode: "move" | "resize", e: KeyboardEvent<HTMLButtonElement>) => {
    const step = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, -1], ArrowDown: [0, 1] }[e.key];
    if (!step) return;
    e.preventDefault();
    const [dx, dy] = step as [number, number];
    edit.onLayout(widget.id, mode === "move" ? moveBy(widget.layout, dx, dy) : resizeBy(widget.layout, dx, dy), true);
  };
  const handle = "absolute z-10 touch-none rounded bg-violet-500/80 text-white hover:bg-violet-600";
  return (
    <>
      <button
        type="button"
        aria-label={`Move ${name}`}
        title="Drag, or use the arrow keys"
        onPointerDown={(e) => down("move", e)}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={cancel}
        onKeyDown={(e) => keys("move", e)}
        className={`${handle} left-1/2 top-1 h-3 w-12 -translate-x-1/2 cursor-move`}
      />
      <button
        type="button"
        aria-label={`Resize ${name}`}
        title="Drag, or use the arrow keys"
        onPointerDown={(e) => down("resize", e)}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={cancel}
        onKeyDown={(e) => keys("resize", e)}
        className={`${handle} bottom-1 right-1 h-3 w-3 cursor-se-resize`}
      />
      {edit.stale.has(widget.id) ? (
        <span role="status" className="absolute right-6 top-1 z-10 rounded bg-zinc-800/80 px-1 text-[10px] text-white">
          Updating preview…
        </span>
      ) : null}
    </>
  );
}
