/**
 * The monitor editor's form, and its translation to and from a definition.
 *
 * The form holds text, not numbers: an empty box means "the server's default",
 * which is different from 0, and a number input that turns "" into 0 would
 * send `renotify_interval_m: 0` for a box nobody touched. So every numeric
 * option is a string here, and only a non-empty one is sent.
 *
 * Validation here is the shallow kind only: required fields and "is this a
 * number". Whether the thresholds are on the right sides of each other, or a
 * mention names a notifier, is the server's to say (validate runs on every
 * pause in typing), because a second copy of those rules here would drift from
 * the one that decides.
 */
import type { MonitorDefinition, MonitorOptions, MonitorType, StoredMonitor } from "./monitorsApi";

/** The editor's state: one string per box. */
export interface MonitorForm {
  uid: string;
  name: string;
  type: MonitorType;
  query: string;
  message: string;
  /** Comma- or space-separated. */
  tags: string;
  enabled: boolean;
  warning: string;
  critical_recovery: string;
  warning_recovery: string;
  evaluation_interval_s: string;
  evaluation_delay_s: string;
  require_full_window: boolean;
  notify_no_data: boolean;
  no_data_timeframe_m: string;
  renotify_interval_m: string;
  min_consecutive_failures: string;
}

/** A new monitor's starting form: a worked example rather than blanks. */
export function emptyForm(): MonitorForm {
  return {
    uid: "",
    name: "",
    type: "metric",
    query: "avg(last_5m):avg:metric.name{*} > 100",
    message: "{{.Monitor}} is {{.Status}} on {{.Group}}: {{.Value}}",
    tags: "",
    enabled: true,
    warning: "",
    critical_recovery: "",
    warning_recovery: "",
    evaluation_interval_s: "",
    evaluation_delay_s: "",
    require_full_window: false,
    notify_no_data: false,
    no_data_timeframe_m: "",
    renotify_interval_m: "",
    min_consecutive_failures: "",
  };
}

const str = (n: number | undefined) => (n === undefined ? "" : String(n));

/** The form for a stored monitor. */
export function formOf(m: StoredMonitor | MonitorDefinition): MonitorForm {
  const o = m.options;
  const t = o.thresholds ?? {};
  return {
    uid: m.uid ?? "",
    name: m.name,
    type: m.type,
    query: m.query,
    message: m.message,
    tags: m.tags.join(", "),
    enabled: m.enabled ?? true,
    warning: str(t.warning),
    critical_recovery: str(t.critical_recovery),
    warning_recovery: str(t.warning_recovery),
    evaluation_interval_s: str(o.evaluation_interval_s),
    evaluation_delay_s: str(o.evaluation_delay_s),
    require_full_window: o.require_full_window ?? false,
    notify_no_data: o.notify_no_data ?? false,
    no_data_timeframe_m: str(o.no_data_timeframe_m),
    renotify_interval_m: str(o.renotify_interval_m),
    min_consecutive_failures: str(o.min_consecutive_failures),
  };
}

/** The result of reading a form: a definition, or why there is none. */
export type FormResult = { ok: true; definition: MonitorDefinition } | { ok: false; errors: string[] };

/**
 * The definition a form describes. The critical threshold is not a box: it is
 * the number at the end of the query, and the server refuses a
 * `thresholds.critical` that differs from it, so a second place to type it
 * could only ever be wrong.
 */
export function definitionOf(f: MonitorForm): FormResult {
  const errors: string[] = [];
  const num = (label: string, v: string, integer: boolean): number | undefined => {
    const s = v.trim();
    if (s === "") return undefined;
    const n = Number(s);
    if (!Number.isFinite(n) || (integer && !Number.isInteger(n))) {
      errors.push(`${label} must be ${integer ? "a whole number" : "a number"}`);
      return undefined;
    }
    return n;
  };
  if (!f.name.trim()) errors.push("name is required");
  if (!f.query.trim()) errors.push("query is required");
  const thresholds = {
    warning: num("warning", f.warning, false),
    critical_recovery: num("critical recovery", f.critical_recovery, false),
    warning_recovery: num("warning recovery", f.warning_recovery, false),
  };
  const options: MonitorOptions = {
    thresholds: dropUndefined(thresholds),
    evaluation_interval_s: num("evaluation interval", f.evaluation_interval_s, true),
    evaluation_delay_s: num("evaluation delay", f.evaluation_delay_s, true),
    require_full_window: f.require_full_window || undefined,
    notify_no_data: f.notify_no_data || undefined,
    no_data_timeframe_m: num("no-data timeframe", f.no_data_timeframe_m, true),
    renotify_interval_m: num("renotify interval", f.renotify_interval_m, true),
    min_consecutive_failures: num("consecutive failures", f.min_consecutive_failures, true),
  };
  if (errors.length) return { ok: false, errors };
  const definition: MonitorDefinition = {
    name: f.name.trim(),
    type: f.type,
    query: f.query.trim(),
    options: dropUndefined(options),
    message: f.message,
    tags: splitTags(f.tags),
    enabled: f.enabled,
  };
  if (f.uid.trim()) definition.uid = f.uid.trim();
  return { ok: true, definition };
}

/**
 * The definition the preview asks about. A blank name blocks saving, not
 * previewing: the preview is about the query, and a new monitor's page
 * should show its line before anyone has thought of a name.
 */
export function previewDefinitionOf(f: MonitorForm): FormResult {
  return definitionOf({ ...f, name: f.name.trim() || "Untitled monitor" });
}

/** Tags from a box: separated by commas or whitespace, de-duplicated, in order. */
export function splitTags(s: string): string[] {
  return [...new Set(s.split(/[\s,]+/).filter(Boolean))];
}

function dropUndefined<T extends object>(o: T): T {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined)) as T;
}
