/**
 * A number input for a field where absent and zero mean different things.
 *
 * The typed text is this component's own state, and the definition is only
 * written when the text *is* something: blank writes "absent", a number in
 * range writes the number, anything else — a lone `-`, `1e`, `11` where the
 * limit is 10 — is left in the box with the reason beside it and not written
 * at all. Writing it would put a `NaN` in the definition, which
 * `JSON.stringify` saves as `null`, which the server reads as zero. The note
 * says which value the definition still holds, since text on its way to being
 * invalid may have been valid a keystroke ago.
 */
import { useId, useState } from "react";
import { readNumber } from "../lib/dashboardEditor";

/** Props for NumberField. */
export interface NumberFieldProps {
  label: string;
  value: number | undefined;
  onChange: (v: number | undefined) => void;
  integer?: boolean;
  min?: number;
  max?: number;
  /** What blank means for this field, shown as the placeholder. */
  placeholder?: string;
  className?: string;
}

const show = (v: number | undefined) => (v === undefined ? "" : String(v));

/** A labelled numeric input that never writes what it cannot read. */
export function NumberField({ label, value, onChange, integer, min, max, placeholder, className = "" }: NumberFieldProps) {
  const id = useId();
  // Text plus the value it was last in step with, so a change from outside
  // (an import, an undo, a drag) replaces the text — adjusting state during
  // render, which React supports for exactly this — while typing does not.
  const [state, setState] = useState({ text: show(value), synced: value });
  let text = state.text;
  if (state.synced !== value) {
    text = show(value);
    setState({ text, synced: value });
  }
  const reading = readNumber(text, { integer, min, max });
  return (
    <div className={`flex flex-col gap-0.5 text-sm ${className}`}>
      <label htmlFor={id} className="text-xs text-zinc-500">
        {label}
      </label>
      <input
        id={id}
        type="text"
        inputMode="decimal"
        value={text}
        placeholder={placeholder}
        aria-invalid={reading.kind === "invalid"}
        onChange={(e) => {
          const next = e.target.value;
          const r = readNumber(next, { integer, min, max });
          const written = r.kind === "number" ? r.value : r.kind === "absent" ? undefined : value;
          setState({ text: next, synced: written });
          if (r.kind !== "invalid" && written !== value) onChange(written);
        }}
        className="w-full rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
      />
      {reading.kind === "invalid" ? (
        <p role="note" className="text-xs text-red-700 dark:text-red-400">
          {/* Says what the definition holds meanwhile — typing "1.5" passed
              through "1", which was written — so the box and the save never
              disagree without the page saying so. */}
          Must be {reading.reason}. Until it is, the dashboard keeps {value === undefined ? "no value here" : value}.
        </p>
      ) : null}
    </div>
  );
}
