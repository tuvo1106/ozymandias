/**
 * The arithmetic behind dragging and resizing widgets on the twelve-column
 * grid.
 *
 * Every function here takes and returns layouts in **cells**, never pixels:
 * the definition is in cells (docs/dashboards.md), and a drag measured in
 * pixels has exactly one place to become cells — [[cellDelta]] — so nothing
 * downstream can store a half-column.
 *
 * The rules are the server's, applied as the author drags rather than
 * refused on save: `w` and `h` at least 1, `x` and `y` not negative, and
 * `x + w` at most 12. A layout that breaks one is clamped here, so a drag can
 * never produce a definition the server would refuse for its geometry.
 *
 * **Collisions push down.** Dropping a widget onto another moves the other
 * one down, and whatever that lands on after it, rather than letting two
 * widgets share cells (CSS grid would draw them on top of each other, and the
 * one underneath is a widget nobody can click) or refusing the drop (which
 * makes rearranging a full dashboard a puzzle). Only *down*: nothing is ever
 * moved sideways or up, so a drop never rearranges the part of the dashboard
 * above the thing that moved.
 */
import { COLUMNS, type Layout, type Widget } from "./dashboard";

const clamp = (v: number, lo: number, hi: number) =>
  Math.min(Math.max(Math.round(v), lo), hi);

/**
 * A layout forced onto the grid: width first (1…12), then x so the widget
 * fits, then height and y. In that order because a widget wider than the
 * space to its right should move left rather than be narrowed — the author
 * chose the width, and the position is the one that was dragged.
 */
export function clampLayout(l: Layout): Layout {
  const w = clamp(l.w, 1, COLUMNS);
  return {
    x: clamp(l.x, 0, COLUMNS - w),
    y: Math.max(Math.round(l.y), 0),
    w,
    h: Math.max(Math.round(l.h), 1),
  };
}

/** A layout moved by whole cells, kept on the grid. Size never changes. */
export function moveBy(l: Layout, dx: number, dy: number): Layout {
  return clampLayout({ ...clampLayout(l), x: l.x + dx, y: l.y + dy });
}

/**
 * A layout resized from its bottom-right corner by whole cells. The top-left
 * never moves, so a resize that would cross the right edge stops at it rather
 * than sliding the widget left.
 */
export function resizeBy(l: Layout, dw: number, dh: number): Layout {
  const base = clampLayout(l);
  return {
    ...base,
    w: clamp(base.w + dw, 1, COLUMNS - base.x),
    h: Math.max(Math.round(base.h + dh), 1),
  };
}

/** Whether two layouts share at least one cell. */
export function overlaps(a: Layout, b: Layout): boolean {
  return a.x < b.x + b.w && b.x < a.x + a.w && a.y < b.y + b.h && b.y < a.y + a.h;
}

/**
 * The widgets with every collision resolved by moving widgets down, the
 * `fixed` one never moving.
 *
 * Widgets are placed in reading order after the fixed one, each at its own
 * `y` or just below whatever already-placed widget it would overlap, so a
 * push cascades. A dashboard that already had overlaps — an imported one, or
 * one saved by an older build — comes out without them too, which is the only
 * consistent thing to do: "no widget hides another" cannot hold for the part
 * of the grid near the drop and not for the rest.
 *
 * Returns the widgets in their original order, as new objects only where the
 * layout changed, so a definition's order (which is the author's, and what a
 * diff of the JSON shows) survives a drag.
 */
export function pushDown(widgets: readonly Widget[], fixed: string | undefined): Widget[] {
  const placed: Layout[] = [];
  const next = new Map<string, Layout>();
  const anchor = widgets.find((w) => w.id === fixed);
  if (anchor) {
    const l = clampLayout(anchor.layout);
    placed.push(l);
    next.set(anchor.id, l);
  }
  const rest = widgets
    .filter((w) => w !== anchor)
    .map((w) => ({ w, l: clampLayout(w.layout) }))
    .sort((a, b) => a.l.y - b.l.y || a.l.x - b.l.x || a.w.id.localeCompare(b.w.id));
  for (const { w, l } of rest) {
    let y = l.y;
    // Each pass moves below one obstacle. `hit` overlaps the widget at `y`,
    // so `hit.y + hit.h > y`: y strictly increases and each obstacle can stop
    // it at most once, so this ends within `placed.length` passes. (A
    // mutation to `hit.y + 1` does not terminate, which is how that was
    // checked.)
    for (;;) {
      const hit = placed.find((p) => overlaps(p, { ...l, y }));
      if (!hit) break;
      y = hit.y + hit.h;
    }
    const moved = { ...l, y };
    placed.push(moved);
    next.set(w.id, moved);
  }
  return widgets.map((w) => {
    const l = next.get(w.id);
    if (!l) return w;
    const same = l.x === w.layout.x && l.y === w.layout.y && l.w === w.layout.w && l.h === w.layout.h;
    return same ? w : { ...w, layout: l };
  });
}

/**
 * Rounds half away from zero. `Math.round` rounds half *up*, so -1.5 becomes
 * -1 while 1.5 becomes 2 — a drag one and a half rows up would move one row
 * and the same drag down would move two. `+ 0` turns the -0 of a small upward
 * drag into 0, which `toEqual` and a layout diff both care about.
 */
function roundHalfAway(v: number): number {
  return Math.sign(v) * Math.round(Math.abs(v)) + 0;
}

/**
 * A pointer movement in pixels as whole cells, rounded to the nearest and
 * symmetrically — see [[roundHalfAway]].
 *
 * A column's pitch is its width plus one gap, and twelve columns have eleven
 * gaps between them — so the pitch is `(width + gap) / 12`, which is the
 * formula that makes the twelfth column land where the grid says it is.
 */
export function cellDelta(
  dxPx: number,
  dyPx: number,
  grid: { width: number; gap: number; rowHeight: number },
): { dx: number; dy: number } {
  const colPitch = (grid.width + grid.gap) / COLUMNS;
  const rowPitch = grid.rowHeight + grid.gap;
  // A grid that has not been laid out (width 0) moves nothing, rather than
  // dividing by zero into an Infinity that clampLayout would then pin to an
  // edge — a drag that teleports the widget to column 12.
  const dx = colPitch > 0 ? roundHalfAway(dxPx / colPitch) : 0;
  const dy = rowPitch > 0 ? roundHalfAway(dyPx / rowPitch) : 0;
  return { dx, dy };
}

/** Where a new widget goes: the left edge, below everything else. */
export function newWidgetLayout(widgets: readonly Widget[], w: number, h: number): Layout {
  const bottom = widgets.reduce((b, x) => Math.max(b, clampLayout(x.layout).y + clampLayout(x.layout).h), 0);
  return clampLayout({ x: 0, y: bottom, w, h });
}
