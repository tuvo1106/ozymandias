import { useState } from "react";
import {
  fromLocalInputValue,
  isRangePreset,
  PRESET_LABELS,
  RANGE_PRESETS,
  resolveTimeRange,
  toLocalInputValue,
  type TimeRange,
} from "../../lib/timeRange";

/** Props for TimeRangePicker. */
export interface TimeRangePickerProps {
  range: TimeRange;
  onChange: (range: TimeRange) => void;
  /** Injected clock, so "Custom" can prefill the current window. */
  now?: () => number;
}

const CUSTOM = "custom";

/**
 * Relative presets plus a custom absolute window. Choosing "Custom…" opens
 * two local-time inputs prefilled with the window currently shown, so
 * narrowing "the last hour" to a fixed incident window is one edit, not
 * two typed timestamps. The absolute range only reaches the URL on Apply.
 */
export function TimeRangePicker({ range, onChange, now = Date.now }: TimeRangePickerProps) {
  const [editing, setEditing] = useState(range.kind === "absolute");
  const initial = resolveTimeRange(range, now());
  const [fromText, setFromText] = useState(toLocalInputValue(initial.from));
  const [toText, setToText] = useState(toLocalInputValue(initial.to));
  const from = fromLocalInputValue(fromText);
  const to = fromLocalInputValue(toText);
  const valid = from !== undefined && to !== undefined && from < to;

  const selectValue = editing ? CUSTOM : range.kind === "relative" ? range.preset : CUSTOM;

  return (
    <div className="flex flex-wrap items-center gap-2">
      <select
        aria-label="Time range"
        value={selectValue}
        onChange={(e) => {
          const v = e.target.value;
          if (isRangePreset(v)) {
            setEditing(false);
            onChange({ kind: "relative", preset: v });
          } else {
            const cur = resolveTimeRange(range, now());
            setFromText(toLocalInputValue(cur.from));
            setToText(toLocalInputValue(cur.to));
            setEditing(true);
          }
        }}
        className="rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900"
      >
        {RANGE_PRESETS.map((p) => (
          <option key={p} value={p}>
            {PRESET_LABELS[p]}
          </option>
        ))}
        <option value={CUSTOM}>Custom…</option>
      </select>
      {editing && (
        <form
          className="flex flex-wrap items-center gap-2 text-sm"
          onSubmit={(e) => {
            e.preventDefault();
            if (valid) onChange({ kind: "absolute", from, to });
          }}
        >
          <input
            type="datetime-local"
            aria-label="From"
            value={fromText}
            onChange={(e) => setFromText(e.target.value)}
            className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
          />
          <span>to</span>
          <input
            type="datetime-local"
            aria-label="To"
            value={toText}
            onChange={(e) => setToText(e.target.value)}
            className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
          />
          <button
            type="submit"
            disabled={!valid}
            className="rounded-md bg-violet-600 px-2 py-1 text-white disabled:bg-zinc-300 dark:disabled:bg-zinc-700"
          >
            Apply
          </button>
          {!valid && <span className="text-red-600 dark:text-red-400">From must be before To.</span>}
        </form>
      )}
    </div>
  );
}
