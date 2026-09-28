/**
 * Small pieces the editor's panels share: a titled section, an optional text
 * field, and the button style.
 */

/** A titled group of fields. */
export function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <fieldset className="flex flex-col gap-2 border-t border-zinc-200 pt-3 dark:border-zinc-800">
      <legend className="pr-2 text-xs font-semibold uppercase tracking-wide text-zinc-500">{title}</legend>
      {children}
    </fieldset>
  );
}

/** A text input where blank means absent. */
export function TextField({
  label,
  value,
  onChange,
  placeholder,
}: {
  label: string;
  value: string | undefined;
  onChange: (v: string | undefined) => void;
  placeholder?: string;
}) {
  return (
    <label className="flex flex-col gap-0.5 text-sm">
      <span className="text-xs text-zinc-500">{label}</span>
      <input
        type="text"
        value={value ?? ""}
        placeholder={placeholder}
        // Empty means absent: an optional text field left blank is not saved
        // as "", which for `name` would be a blank column header.
        onChange={(e) => onChange(e.target.value === "" ? undefined : e.target.value)}
        className="rounded-md border border-zinc-300 bg-white px-2 py-1 dark:border-zinc-700 dark:bg-zinc-900"
      />
    </label>
  );
}

/** The editor's small secondary button. */
export const button =
  "rounded-md border border-zinc-300 px-2 py-1 text-xs hover:bg-zinc-100 dark:border-zinc-700 dark:hover:bg-zinc-800";

