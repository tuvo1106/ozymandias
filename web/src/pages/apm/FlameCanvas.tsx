import { useEffect, useRef, useState } from "react";
import { formatDuration, hitTest, panView, serviceColor, viewOf, zoomView, type FlameLayout, type View } from "../../lib/flame";

/** Row height in CSS px. */
export const ROW_H = 20;
const MIN_BAR_PX = 1;
const LABEL_MIN_PX = 44;

/** Props for FlameCanvas. */
export interface FlameCanvasProps {
  layout: FlameLayout;
  view: View;
  onView: (v: View) => void;
  selected: string | undefined;
  onSelect: (id: string | undefined) => void;
  /** Dim everything off the critical path. */
  critical: boolean;
}

function prefersDark(): boolean {
  return typeof window !== "undefined" && typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

/**
 * The flame graph, painted on a canvas: a trace can hold thousands of spans
 * and a DOM node each would make zoom and pan stutter. The layout module
 * decided every rect; this only maps fractions to pixels (through the zoom
 * window), paints, and turns pointer events back into fractions for
 * hit-testing.
 *
 * Gestures: wheel zooms about the cursor, drag pans, a click selects, a
 * double-click zooms to the span, and a click on empty space deselects. The
 * canvas is not keyboard-navigable; the waterfall view is the accessible
 * alternative to it and the page offers it one click away.
 */
export function FlameCanvas({ layout, view, onView, selected, onSelect, critical }: FlameCanvasProps) {
  const wrap = useRef<HTMLDivElement>(null);
  const canvas = useRef<HTMLCanvasElement>(null);
  const [width, setWidth] = useState(900);
  const [dragging, setDragging] = useState(false);
  const [tip, setTip] = useState<{ x: number; y: number; text: string } | undefined>();
  const drag = useRef<{ x: number; view: View; moved: boolean } | undefined>(undefined);
  const viewRef = useRef(view);
  useEffect(() => {
    viewRef.current = view;
  });
  const height = Math.max(1, layout.rows) * ROW_H;

  useEffect(() => {
    const el = wrap.current;
    if (!el) return;
    setWidth(Math.max(200, el.clientWidth || 900));
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => setWidth(Math.max(200, el.clientWidth || 900)));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // A wheel listener must be non-passive to stop the page scrolling under a zoom, and React's onWheel is passive.
  useEffect(() => {
    const el = canvas.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      const v = viewRef.current;
      const rect = el.getBoundingClientRect();
      const focus = v.x0 + ((e.clientX - rect.left) / (rect.width || 1)) * (v.x1 - v.x0);
      onView(zoomView(v, focus, e.deltaY < 0 ? 1.25 : 0.8));
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
  }, [onView]);

  const dark = prefersDark();
  useEffect(() => {
    const el = canvas.current;
    const ctx = el?.getContext("2d");
    if (!el || !ctx) return; // no 2d context (jsdom): nothing to paint
    const dpr = window.devicePixelRatio || 1;
    el.width = Math.round(width * dpr);
    el.height = Math.round(height * dpr);
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, width, height);
    const colors = new Map<string, string>(); // a hash per rect per paint adds up at 5000 spans
    const colorOf = (service: string) => {
      let c = colors.get(service);
      if (!c) colors.set(service, (c = serviceColor(service, dark)));
      return c;
    };
    const span = view.x1 - view.x0 || 1;
    const px = (f: number) => ((f - view.x0) / span) * width;
    ctx.font = "11px ui-sans-serif, system-ui, sans-serif";
    ctx.textBaseline = "middle";

    // Queue-wait gaps first, so a bar drawn over their edge wins.
    const hatch = document.createElement("canvas");
    hatch.width = hatch.height = 8;
    const h = hatch.getContext("2d");
    let pattern: CanvasPattern | null = null;
    if (h) {
      h.strokeStyle = dark ? "#a1a1aa" : "#71717a";
      h.lineWidth = 1;
      h.beginPath();
      h.moveTo(0, 8);
      h.lineTo(8, 0);
      h.stroke();
      pattern = ctx.createPattern(hatch, "repeat");
    }
    for (const g of layout.gaps) {
      const x = px(g.x);
      const w = px(g.x + g.w) - x;
      if (x + w < 0 || x > width) continue;
      const y = g.row * ROW_H + 1;
      ctx.fillStyle = pattern ?? "#a1a1aa";
      ctx.fillRect(x, y, Math.max(w, MIN_BAR_PX), ROW_H - 2);
      ctx.strokeStyle = dark ? "#71717a" : "#a1a1aa";
      ctx.strokeRect(x + 0.5, y + 0.5, Math.max(w, MIN_BAR_PX) - 1, ROW_H - 3);
    }

    for (const r of layout.rects) {
      const x = px(r.x);
      const w = Math.max(px(r.x + r.w) - x, MIN_BAR_PX);
      if (x + w < 0 || x > width) continue;
      const y = r.row * ROW_H + 1;
      const dim = critical && !r.critical;
      ctx.globalAlpha = dim ? 0.35 : 1;
      if (r.synthetic) {
        ctx.fillStyle = dark ? "#3f3f46" : "#e4e4e7";
        ctx.fillRect(x, y, w, ROW_H - 2);
        ctx.setLineDash([4, 3]);
        ctx.strokeStyle = "#a1a1aa";
        ctx.strokeRect(x + 0.5, y + 0.5, w - 1, ROW_H - 3);
        ctx.setLineDash([]);
      } else {
        ctx.fillStyle = colorOf(r.span!.service);
        ctx.fillRect(x, y, w, ROW_H - 2);
      }
      if (r.error) {
        ctx.strokeStyle = "#dc2626";
        ctx.lineWidth = 2;
        ctx.strokeRect(x + 1, y + 1, Math.max(w - 2, 1), ROW_H - 4);
        ctx.lineWidth = 1;
      } else if (critical && r.critical) {
        ctx.strokeStyle = dark ? "#fafafa" : "#18181b";
        ctx.lineWidth = 2;
        ctx.strokeRect(x + 1, y + 1, Math.max(w - 2, 1), ROW_H - 4);
        ctx.lineWidth = 1;
      }
      if (r.clamped) {
        // A small notch at the left edge: this bar was pulled inside its parent (clock skew).
        ctx.fillStyle = "#f59e0b";
        ctx.fillRect(x, y, 3, 3);
      }
      if (r.id === selected) {
        ctx.strokeStyle = "#7c3aed";
        ctx.lineWidth = 2;
        ctx.strokeRect(x, y, Math.max(w, 2), ROW_H - 2);
        ctx.lineWidth = 1;
      }
      if (w >= LABEL_MIN_PX) {
        ctx.globalAlpha = dim ? 0.6 : 1;
        ctx.fillStyle = r.synthetic ? (dark ? "#e4e4e7" : "#3f3f46") : "#fff";
        const label = r.synthetic ? "missing parent" : r.span!.name;
        const text = `${r.collapsed ? `+${r.hidden} ` : ""}${label}  ${r.synthetic ? "" : formatDuration(r.end - r.start)}`;
        ctx.save();
        ctx.beginPath();
        ctx.rect(x + 2, y, w - 4, ROW_H - 2);
        ctx.clip();
        ctx.fillText(text, Math.max(x, 0) + 4, y + (ROW_H - 2) / 2);
        ctx.restore();
      }
      ctx.globalAlpha = 1;
    }
  }, [layout, view, width, height, selected, critical, dark]);

  const toFrac = (clientX: number): number => {
    const rect = canvas.current?.getBoundingClientRect();
    return view.x0 + ((clientX - (rect?.left ?? 0)) / (rect?.width || width)) * (view.x1 - view.x0);
  };
  const slack = (2 / width) * (view.x1 - view.x0); // 2px, so a zero-width span can be pointed at
  const rowAt = (clientY: number) => Math.floor((clientY - (canvas.current?.getBoundingClientRect().top ?? 0)) / ROW_H);
  const gapAt = (xf: number, row: number) => layout.gaps.find((g) => g.row === row && xf >= g.x && xf <= g.x + g.w);

  return (
    <div ref={wrap} className="relative w-full overflow-hidden rounded border border-zinc-200 dark:border-zinc-800">
      <canvas
        ref={canvas}
        role="img"
        aria-label={`Flame graph of ${layout.rects.length} spans. Use the waterfall view for keyboard access.`}
        style={{ width: "100%", height, display: "block", cursor: dragging ? "grabbing" : "default" }}
        onPointerDown={(e) => {
          drag.current = { x: e.clientX, view, moved: false };
          setDragging(true);
          e.currentTarget.setPointerCapture?.(e.pointerId);
        }}
        onPointerMove={(e) => {
          const d = drag.current;
          if (d) {
            const dx = e.clientX - d.x;
            if (Math.abs(dx) > 3) d.moved = true;
            if (d.moved) {
              onView(panView(d.view, (-dx / width) * (d.view.x1 - d.view.x0)));
              setTip(undefined);
              return;
            }
          }
          const rect = canvas.current?.getBoundingClientRect();
          const xf = toFrac(e.clientX);
          const row = rowAt(e.clientY);
          const hit = hitTest(layout.rects, xf, row, slack);
          const gap = hit ? undefined : gapAt(xf, row);
          const text = hit
            ? hit.synthetic
              ? "Missing parent: its spans were not stored"
              : `${hit.span!.service} · ${hit.span!.name} · ${hit.span!.resource}\n${formatDuration(hit.end - hit.start)}${hit.clamped ? " (clamped into parent: clock skew)" : ""}`
            : gap
              ? `Queue wait ${formatDuration(gap.waitUs)}`
              : undefined;
          setTip(text ? { x: e.clientX - (rect?.left ?? 0) + 12, y: e.clientY - (rect?.top ?? 0) + 12, text } : undefined);
        }}
        onPointerUp={(e) => {
          const d = drag.current;
          drag.current = undefined;
          if (d?.moved) return;
          onSelect(hitTest(layout.rects, toFrac(e.clientX), rowAt(e.clientY), slack)?.id);
        }}
        onPointerLeave={() => {
          drag.current = undefined;
          setTip(undefined);
        }}
        onDoubleClick={(e) => {
          const hit = hitTest(layout.rects, toFrac(e.clientX), rowAt(e.clientY), slack);
          if (hit) onView(viewOf(hit));
        }}
      />
      {tip && (
        <div role="tooltip" className="pointer-events-none absolute z-10 max-w-sm whitespace-pre-wrap rounded bg-zinc-900 px-2 py-1 text-xs text-white shadow dark:bg-zinc-100 dark:text-zinc-900" style={{ left: tip.x, top: tip.y }}>
          {tip.text}
        </div>
      )}
    </div>
  );
}
