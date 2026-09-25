/**
 * A text input with a suggestion list: the ARIA "combobox with listbox
 * popup" pattern, kept deliberately small.
 *
 * The component is controlled and knows nothing about where suggestions come
 * from: the parent owns the typed text, fetches (debounced) suggestions for it
 * and passes them in. That keeps server state in TanStack Query and leaves
 * this file pure UI. Arrow keys move the highlight, Enter picks the
 * highlighted suggestion (or submits the typed text when none is
 * highlighted, so a name the server doesn't know yet can still be entered,
 * e.g. a `*` wildcard), Escape closes the list.
 */
import { useId, useState, type KeyboardEvent } from "react";

/** Props for Autocomplete. */
export interface AutocompleteProps {
  /** Accessible name of the input. */
  label: string;
  /** The typed text. */
  value: string;
  /** Called on every keystroke with the new text. */
  onChange: (text: string) => void;
  /** Suggestions for the current text, best first. */
  options: readonly string[];
  /** Called with the chosen suggestion, or the typed text on Enter. */
  onSubmit: (value: string) => void;
  placeholder?: string;
  /** Shown instead of the list while suggestions load. */
  loading?: boolean;
  autoFocus?: boolean;
  /** Called on Escape when the list is already closed. */
  onCancel?: () => void;
  /** Called when focus leaves the input (not when a suggestion is clicked). */
  onBlur?: () => void;
  className?: string;
}

/** A combobox input with keyboard-navigable suggestions. */
export function Autocomplete({
  label,
  value,
  onChange,
  options,
  onSubmit,
  placeholder,
  loading = false,
  autoFocus = false,
  onCancel,
  onBlur,
  className = "",
}: AutocompleteProps) {
  const listId = useId();
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const shown = open && (loading || options.length > 0);

  const choose = (v: string) => {
    setOpen(false);
    setActive(-1);
    onSubmit(v);
  };

  const onKeyDown = (e: KeyboardEvent<HTMLInputElement>) => {
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      e.preventDefault();
      setOpen(true);
      if (options.length === 0) return;
      const step = e.key === "ArrowDown" ? 1 : -1;
      setActive((a) => (a + step + options.length) % options.length);
    } else if (e.key === "Enter") {
      e.preventDefault();
      const picked = shown && active >= 0 ? options[active] : undefined;
      const v = picked ?? value.trim();
      if (v) choose(v);
    } else if (e.key === "Escape") {
      if (shown) setOpen(false);
      else onCancel?.();
    }
  };

  return (
    <div className={`relative ${className}`}>
      <input
        type="text"
        role="combobox"
        aria-label={label}
        aria-expanded={shown}
        aria-controls={listId}
        aria-autocomplete="list"
        aria-activedescendant={shown && active >= 0 ? `${listId}-${active}` : undefined}
        autoComplete="off"
        autoFocus={autoFocus}
        placeholder={placeholder}
        value={value}
        onChange={(e) => {
          onChange(e.target.value);
          setOpen(true);
          setActive(-1);
        }}
        onFocus={() => setOpen(true)}
        onBlur={() => {
          setOpen(false);
          onBlur?.();
        }}
        onKeyDown={onKeyDown}
        className="w-full rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm outline-none focus:border-violet-500 dark:border-zinc-700 dark:bg-zinc-900"
      />
      {shown && (
        <ul
          id={listId}
          role="listbox"
          aria-label={`${label} suggestions`}
          className="absolute z-10 mt-1 max-h-64 w-full min-w-48 overflow-auto rounded-md border border-zinc-200 bg-white py-1 text-sm shadow-lg dark:border-zinc-700 dark:bg-zinc-900"
        >
          {loading && options.length === 0 ? (
            <li className="px-2 py-1 text-zinc-500">Loading…</li>
          ) : (
            options.map((o, i) => (
              <li
                key={o}
                id={`${listId}-${i}`}
                role="option"
                aria-selected={i === active}
                // mousedown, not click: it fires before the input's blur
                // closes the list, and preventDefault keeps focus put.
                onMouseDown={(e) => {
                  e.preventDefault();
                  choose(o);
                }}
                className={`cursor-pointer truncate px-2 py-1 ${
                  i === active ? "bg-violet-100 dark:bg-violet-500/20" : "hover:bg-zinc-100 dark:hover:bg-zinc-800"
                }`}
              >
                {o}
              </li>
            ))
          )}
        </ul>
      )}
    </div>
  );
}
