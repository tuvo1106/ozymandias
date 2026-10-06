/**
 * Flame-graph layout: spans in, rectangles out. Pure and DOM-free, so the
 * hard cases (overlapping siblings, orphans, clock skew) are unit tests and
 * the canvas only has to paint what this returns.
 *
 * The mental model. A trace is a forest, not a tree: spans arrive from several
 * processes with independent clocks, and a sampled store may hold a child whose
 * parent it never saw. A layout must therefore never throw on, and never lose,
 * a span it is given:
 *  - a span whose parent is absent is an *orphan*. It hangs under a synthetic
 *    "missing parent" node (one per missing id) so it is visible and labelled
 *    instead of silently becoming a second root;
 *  - a span in a parent cycle (corrupt data) is treated as an orphan too;
 *  - a child that starts or ends outside its parent (clock skew between hosts)
 *    is clamped inside it, and the rect says so (`clamped`), because a bar
 *    that overflows its parent reads as a bug in the graph, while a clamp
 *    that is hidden reads as a fact about the code. The one exception is a
 *    consumer span under a producer (arq.enqueue -> arq.job): it is meant to
 *    start after its parent ended, and the distance is the queue wait, drawn
 *    as its own `gap`.
 *
 * Rows. Siblings that do not overlap share one row (a sequence of calls looks
 * like a sequence). Siblings that overlap (parallel work) are packed into
 * lanes, each lane below the previous one's whole subtree, so no two rects
 * ever occupy the same cell. This is the rejected alternative to "every
 * span its own row", which is the waterfall and is also offered.
 *
 * x and width are fractions of the trace's duration, in [0, 1]; the canvas
 * multiplies by pixels, and zoom is a window over the same fractions.
 * Everything is iterative: a trace 5000 deep must not overflow the stack.
 */

/** The fields of a span the layout reads; the wire span has more. */
export interface FlameSpan {
  span_id: string;
  parent_id?: string | null;
  service: string;
  name: string;
  resource: string;
  /** Unix microseconds. */
  start: number;
  /** Microseconds. */
  duration: number;
  error: number;
  type?: string;
  meta?: Record<string, string>;
  metrics?: Record<string, number>;
}

/** One painted bar, or the stand-in for a missing parent. */
export interface FlameRect<S extends FlameSpan = FlameSpan> {
  /** The span id, or `missing:<parent id>` for a synthetic node. */
  id: string;
  /** Absent only for a synthetic node. */
  span?: S;
  synthetic: boolean;
  parentId: string | undefined;
  /** Tree depth: 0 for a root. Indents the waterfall. */
  level: number;
  /** Flame row: equals `level` unless overlapping siblings pushed it down. */
  row: number;
  /** Left edge, fraction of the trace duration. */
  x: number;
  /** Width, fraction of the trace duration; 0 for a zero-duration span. */
  w: number;
  /** Unix microseconds after clamping. */
  start: number;
  end: number;
  /** The span's own interval was outside its parent's and was pulled in. */
  clamped: boolean;
  /** On the critical path: the chain of last-finishing children from the root. */
  critical: boolean;
  /** Subtree is folded away. */
  collapsed: boolean;
  /** Descendants hidden by the fold (0 unless `collapsed`). */
  hidden: number;
  /** Direct children, whether shown or folded. */
  childCount: number;
  error: boolean;
}

/** The wait between a producer span ending and its consumer starting. */
export interface QueueGap {
  /** The consumer (`arq.job`) span id. */
  jobId: string;
  /** The producer (`arq.enqueue`) span id. */
  enqueueId: string;
  row: number;
  x: number;
  w: number;
  /** Microseconds waited. */
  waitUs: number;
}

/** What [[layoutTrace]] returns. */
export interface FlameLayout<S extends FlameSpan = FlameSpan> {
  /** Unix microseconds of the earliest start. */
  start: number;
  /** Microseconds from the earliest start to the latest end (at least 1). */
  duration: number;
  /** Rects in pre-order: parents before children, siblings by start. Also the waterfall's row order. */
  rects: FlameRect<S>[];
  gaps: QueueGap[];
  /** Rows the flame needs (max row + 1). */
  rows: number;
  /** How many spans had to be clamped into their parents. */
  clamped: number;
  /** Number of synthetic "missing parent" nodes. */
  missing: number;
}

/** Layout options. */
export interface LayoutOptions {
  /** Span ids (or synthetic ids) whose subtrees are folded into one bar. */
  collapsed?: ReadonlySet<string>;
}

/** Span names whose child is a consumer that is *expected* to start late. */
const PRODUCER = "arq.enqueue";
const CONSUMER = "arq.job";

interface Node<S extends FlameSpan> {
  id: string;
  span?: S;
  parent?: Node<S>;
  children: Node<S>[];
  start: number;
  end: number;
  clamped: boolean;
  lane: number;
  height: number;
  laneOffsets: number[];
  row: number;
  level: number;
  descendants: number;
}

/** Id of the synthetic node standing in for a parent the store does not hold. */
export const missingId = (parentId: string): string => `missing:${parentId}`;

function newNode<S extends FlameSpan>(id: string, span: S | undefined, start: number, end: number): Node<S> {
  return { id, span, children: [], start, end, clamped: false, lane: 0, height: 1, laneOffsets: [], row: 0, level: 0, descendants: 0 };
}

/**
 * Lays a trace out. Spans with a repeated id keep the first; an empty list
 * gives an empty layout. See the module comment for the rules.
 */
export function layoutTrace<S extends FlameSpan>(spans: readonly S[], opts: LayoutOptions = {}): FlameLayout<S> {
  if (spans.length === 0) return { start: 0, duration: 1, rects: [], gaps: [], rows: 0, clamped: 0, missing: 0 };

  const byId = new Map<string, Node<S>>();
  for (const s of spans) {
    if (!byId.has(s.span_id)) byId.set(s.span_id, newNode(s.span_id, s, s.start, s.start + Math.max(0, s.duration)));
  }

  // Link children to parents. A span is a root, or an orphan (parent unknown, or itself).
  const roots: Node<S>[] = [];
  const synthetic = new Map<string, Node<S>>();
  const attachOrphan = (n: Node<S>, parentId: string) => {
    let m = synthetic.get(parentId);
    if (!m) {
      m = newNode<S>(missingId(parentId), undefined, 0, 0);
      synthetic.set(parentId, m);
      roots.push(m);
    }
    n.parent = m;
    m.children.push(n);
  };
  for (const n of byId.values()) {
    const pid = n.span?.parent_id;
    if (!pid) {
      roots.push(n);
      continue;
    }
    const p = pid === n.id ? undefined : byId.get(pid);
    if (p) {
      n.parent = p;
      p.children.push(n);
    } else attachOrphan(n, pid);
  }
  // A cycle has no root above it, so a walk from the roots never reaches it:
  // whatever it misses is re-attached as an orphan, which breaks the cycle.
  const reach = new Set<Node<S>>();
  const walk = (from: Node<S>[]) => {
    const stack = [...from];
    for (let n = stack.pop(); n; n = stack.pop()) {
      if (reach.has(n)) continue;
      reach.add(n);
      stack.push(...n.children);
    }
  };
  walk(roots);
  for (const n of byId.values()) {
    if (reach.has(n)) continue;
    const pid = n.span?.parent_id ?? n.id;
    n.parent?.children.splice(n.parent.children.indexOf(n), 1);
    attachOrphan(n, pid);
    walk([n]);
  }

  // A synthetic node spans its orphans; its extent is set before sorting.
  for (const m of synthetic.values()) {
    m.start = Math.min(...m.children.map((c) => c.start));
    m.end = Math.max(...m.children.map((c) => c.end));
  }

  const order = (a: Node<S>, b: Node<S>) => a.start - b.start || a.end - b.end || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  roots.sort(order);

  // Pre-order (parents first). Siblings are sorted by start here, once.
  const pre: Node<S>[] = [];
  const stack = [...roots].reverse();
  for (let n = stack.pop(); n; n = stack.pop()) {
    pre.push(n);
    n.children.sort(order);
    for (let i = n.children.length - 1; i >= 0; i--) stack.push(n.children[i]!);
  }

  let t0 = Infinity;
  let t1 = -Infinity;
  for (const n of pre) {
    t0 = Math.min(t0, n.start);
    t1 = Math.max(t1, n.end);
  }
  const duration = Math.max(1, t1 - t0);

  // Pass 1, top-down: clamp each child inside its (already clamped) parent.
  let clampedCount = 0;
  for (const n of pre) {
    const p = n.parent;
    if (!p || p.span === undefined) continue; // roots, and orphans (their parent has no time of its own)
    if (n.span?.name === CONSUMER && p.span.name === PRODUCER) continue; // queue wait is by design
    const s = Math.min(Math.max(n.start, p.start), p.end);
    const e = Math.min(Math.max(n.end, s), p.end);
    if (s !== n.start || e !== n.end) {
      n.clamped = true;
      clampedCount++;
    }
    n.start = s;
    n.end = e;
  }

  // Pass 2, bottom-up: lanes for overlapping siblings, and subtree heights.
  const folded = (n: Node<S>) => opts.collapsed?.has(n.id) === true;
  const packLanes = (items: Node<S>[]): number[] => {
    const laneEnd: number[] = [];
    const laneHeight: number[] = [];
    for (const c of items) {
      let l = laneEnd.findIndex((e) => e <= c.start);
      if (l < 0) l = laneEnd.length;
      laneEnd[l] = c.end;
      laneHeight[l] = Math.max(laneHeight[l] ?? 0, c.height);
      c.lane = l;
    }
    const offsets: number[] = [];
    let acc = 0;
    for (const h of laneHeight) {
      offsets.push(acc);
      acc += h;
    }
    offsets.push(acc); // the total rides along as the last element
    return offsets;
  };
  for (let i = pre.length - 1; i >= 0; i--) {
    const n = pre[i]!;
    n.descendants = n.children.reduce((a, c) => a + 1 + c.descendants, 0);
    if (folded(n) || n.children.length === 0) continue;
    const offsets = packLanes(n.children);
    n.height = 1 + offsets.pop()!;
    n.laneOffsets = offsets;
  }

  // Pass 3, top-down: rows. Roots are packed like the lanes of an imaginary parent.
  const rootOffsets = packLanes(roots);
  rootOffsets.pop();
  for (const r of roots) r.row = rootOffsets[r.lane]!;
  for (const n of pre) {
    if (n.parent) {
      n.row = n.parent.row + 1 + (n.parent.laneOffsets[n.lane] ?? 0);
      n.level = n.parent.level + 1;
    }
  }

  // Critical path: from the root that ends last, follow the child that ends last.
  const later = (a: Node<S>, b: Node<S>) => (b.end > a.end || (b.end === a.end && b.end - b.start > a.end - a.start) ? b : a);
  const critical = new Set<Node<S>>();
  for (let cur: Node<S> | undefined = roots.reduce(later); cur; cur = cur.children.length ? cur.children.reduce(later) : undefined) {
    critical.add(cur);
  }

  // Emit, skipping everything under a fold.
  const rects: FlameRect<S>[] = [];
  const gaps: QueueGap[] = [];
  const hidden = new Set<Node<S>>();
  let maxRow = 0;
  for (const n of pre) {
    if (n.parent && (hidden.has(n.parent) || folded(n.parent))) {
      hidden.add(n);
      continue;
    }
    const isFolded = folded(n) && n.children.length > 0;
    maxRow = Math.max(maxRow, n.row);
    rects.push({
      id: n.id,
      span: n.span,
      synthetic: n.span === undefined,
      parentId: n.parent?.id,
      level: n.level,
      row: n.row,
      x: (n.start - t0) / duration,
      w: (n.end - n.start) / duration,
      start: n.start,
      end: n.end,
      clamped: n.clamped,
      critical: critical.has(n),
      collapsed: isFolded,
      hidden: isFolded ? n.descendants : 0,
      childCount: n.children.length,
      error: (n.span?.error ?? 0) !== 0,
    });
    const p = n.parent;
    if (n.span?.name === CONSUMER && p?.span?.name === PRODUCER && n.start > p.end) {
      gaps.push({ jobId: n.id, enqueueId: p.id, row: n.row, x: (p.end - t0) / duration, w: (n.start - p.end) / duration, waitUs: n.start - p.end });
    }
  }
  return { start: t0, duration, rects, gaps, rows: maxRow + 1, clamped: clampedCount, missing: synthetic.size };
}

/** A visible window over the trace, as fractions: `0 <= x0 < x1 <= 1`. */
export interface View {
  x0: number;
  x1: number;
}

/** The whole trace. */
export const FULL_VIEW: View = { x0: 0, x1: 1 };

/** Narrowest window zoom allows: below this a bar is sub-pixel noise and float error. */
const MIN_SPAN = 1e-6;

/**
 * Zooms by `factor` (> 1 zooms in) keeping the point at `focus` (a fraction
 * of the trace) under the cursor, then keeps the window inside the trace.
 */
export function zoomView(v: View, focus: number, factor: number): View {
  const span = Math.min(1, Math.max(MIN_SPAN, (v.x1 - v.x0) / factor));
  const f = (Math.min(Math.max(focus, v.x0), v.x1) - v.x0) / (v.x1 - v.x0 || 1);
  const x0 = Math.min(Math.max(0, focus - f * span), 1 - span);
  return { x0, x1: x0 + span };
}

/** Shifts the window by `dx` (a fraction of the trace), stopping at the edges. */
export function panView(v: View, dx: number): View {
  const span = v.x1 - v.x0;
  const x0 = Math.min(Math.max(0, v.x0 + dx), 1 - span);
  return { x0, x1: x0 + span };
}

/** The window that frames a rect, with a little air; used by double-click to zoom to a span. */
export function viewOf(r: { x: number; w: number }): View {
  const pad = Math.max(r.w * 0.05, MIN_SPAN);
  const x0 = Math.max(0, r.x - pad);
  const x1 = Math.min(1, Math.max(r.x + r.w + pad, x0 + MIN_SPAN));
  return { x0, x1 };
}

/** Finds the rect under a point (`xFrac` of the trace, flame `row`), preferring the narrowest. */
export function hitTest<S extends FlameSpan>(rects: readonly FlameRect<S>[], xFrac: number, row: number, slack = 0): FlameRect<S> | undefined {
  let best: FlameRect<S> | undefined;
  for (const r of rects) {
    if (r.row !== row || xFrac < r.x - slack || xFrac > r.x + r.w + slack) continue;
    if (!best || r.w < best.w) best = r;
  }
  return best;
}

/** Microseconds as a short human duration: `850µs`, `12.4ms`, `1.20s`, `—` for a non-number. */
export function formatDuration(us: number | null | undefined): string {
  if (us === null || us === undefined || !Number.isFinite(us)) return "—";
  if (us < 1000) return `${Math.round(us)}µs`;
  if (us < 1_000_000) return `${(us / 1000).toFixed(us < 10_000 ? 2 : 1)}ms`;
  return `${(us / 1_000_000).toFixed(2)}s`;
}

/** A stable colour per service: the hash picks the hue, so a service is the same colour on every page. */
export function serviceColor(service: string, dark = false): string {
  let h = 2166136261;
  for (let i = 0; i < service.length; i++) h = Math.imul(h ^ service.charCodeAt(i), 16777619);
  const hue = (h >>> 0) % 360;
  return `hsl(${hue} ${dark ? 55 : 60}% ${dark ? 45 : 55}%)`;
}
