/**
 * A select over a fixed set of values that can also show the two things a
 * plain `<select>` cannot: *no value*, and *a value this build does not know*.
 *
 * The browser's behaviour is the reason this exists. A controlled `<select>`
 * whose value matches none of its options **displays its first option** — so
 * a reducer stored as `"p42"` would read as "last" in the editor while the
 * definition still said `"p42"`, and saving would send the value nobody saw.
 * Here an unknown value gets an option of its own, labelled as what it is and
 * selected, plus a sentence saying so; absence gets its own option too, with a
 * label that says what absence *means* for this field ("line (the default)",
 * or "choose one" where the field is required and nothing is assumed).
 */
import { useId } from "react";
import type { EnumReading } from "../lib/dashboardEditor";

/** Props for EnumSelect. */
export interface EnumSelectProps<T extends string> {
  label: string;
  reading: EnumReading<T>;
  options: readonly T[];
  /** A person's label for each option; the value itself when omitted. */
  optionLabel?: (v: T) => string;
  /**
   * What the absent state is called, and whether the author may choose it.
   * Omitted: the field is required, absent shows as "choose one" and cannot
   * be chosen back once something is.
   */
  absent?: { label: string };
  /** Called with a chosen value, or undefined for the absent option. */
  onChange: (v: T | undefined) => void;
  className?: string;
}

const ABSENT = "\u0000absent";
const UNKNOWN = "\u0000unknown";

/** A labelled select that renders absent and unknown values as themselves. */
export function EnumSelect<T extends string>({
  label,
  reading,
  options,
  optionLabel = (v) => v,
  absent,
  onChange,
  className = "",
}: EnumSelectProps<T>) {
  const id = useId();
  const value = reading.kind === "known" ? reading.value : reading.kind === "unknown" ? UNKNOWN : ABSENT;
  const required = absent === undefined;
  return (
    <div className={`flex flex-col gap-0.5 text-sm ${className}`}>
      <label htmlFor={id} className="text-xs text-zinc-500">
        {label}
      </label>
      <select
        id={id}
        value={value}
        aria-invalid={reading.kind === "unknown" || (required && reading.kind === "absent")}
        onChange={(e) => {
          const v = e.target.value;
          if (v === UNKNOWN) return; // re-selecting what is already there
          onChange(v === ABSENT ? undefined : (v as T));
        }}
        className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
      >
        {absent ? (
          <option value={ABSENT}>{absent.label}</option>
        ) : reading.kind === "absent" ? (
          <option value={ABSENT} disabled>
            Choose one
          </option>
        ) : null}
        {reading.kind === "unknown" ? (
          <option value={UNKNOWN}>{`"${reading.raw}" (not known to this build)`}</option>
        ) : null}
        {options.map((o) => (
          <option key={o} value={o}>
            {optionLabel(o)}
          </option>
        ))}
      </select>
      {reading.kind === "unknown" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          {`This build does not know "${reading.raw}". It is kept as it is until you choose something else.`}
        </p>
      ) : required && reading.kind === "absent" ? (
        <p role="note" className="text-xs text-amber-700 dark:text-amber-500">
          Required, and not chosen for you.
        </p>
      ) : null}
    </div>
  );
}
