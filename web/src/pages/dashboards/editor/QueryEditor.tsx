/**
 * The metricql editor: a text box that completes what is being typed and
 * underlines where ozyd says the query stops parsing.
 *
 * Three parts, each owned elsewhere so this file is only wiring:
 * [[completionContext]] decides what position the caret is in,
 * [[useCompletions]] decides what goes there (and whether it can know yet),
 * and [[useQueryValidation]] asks `POST /api/v1/query/validate` about the
 * text. What this file adds is the rendering, and the rule that each state
 * those return has its own picture: a list that is loading does not look
 * like a list that is empty, and a query nobody could check does not look
 * like one that parses.
 *
 * A textarea rather than an input because the grammar allows a query to span
 * lines, and a long filter is easier to read that way. Enter inserts a
 * newline unless a suggestion is highlighted; Tab and Enter take the
 * highlighted one; Ctrl+Space opens the list where it would not open itself.
 */
import { useId, useLayoutEffect, useRef, useState, type KeyboardEvent } from "react";
import { applyCompletion, type Completion, type CompletionList } from "../../../lib/completions";
import { completionContext } from "../../../lib/queryContext";
import { lineAndColumn, type ValidationState } from "../../../lib/queryValidation";
import { useCompletions, useQueryValidation } from "../../../lib/useQueryEditor";

/** Props for QueryEditor. */
export interface QueryEditorProps {
  label: string;
  value: string;
  onChange: (q: string) => void;
  /** The dashboard's template variable names, for `$` completion. */
  variables: readonly string[];
  /**
   * Called on Ctrl+Enter (⌘+Enter on a Mac), for a page where the query is
   * run rather than followed. Plain Enter stays a newline, or a choice when
   * the list is open: a query can span lines, and a key that sometimes
   * inserts and sometimes runs would run half-written queries.
   */
  onSubmit?: () => void;
}

/** A metricql text box with completion and inline parse errors. */
export function QueryEditor({ label, value, onChange, variables, onSubmit }: QueryEditorProps) {
  const id = useId();
  const listId = `${id}-list`;
  const ref = useRef<HTMLTextAreaElement>(null);
  const [caret, setCaret] = useState(value.length);
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(-1);
  const ctx = completionContext(value, caret);
  const list = useCompletions(ctx, variables);
  const verdict = useQueryValidation(value);
  const items = list.status === "ready" ? list.items : [];
  // "none" draws no popup at all; every other state draws one that says
  // what it is, so an empty popup never stands in for "nothing exists".
  const shown = open && list.status !== "none";

  // Where the caret goes once a chosen completion's text is on screen. Set in
  // a layout effect keyed on the value, so it happens in the same commit as
  // the text — a requestAnimationFrame could fire after the next keystroke
  // and pull the caret back into the middle of what was just typed.
  const pendingCaret = useRef<number | null>(null);
  useLayoutEffect(() => {
    const at = pendingCaret.current;
    if (at === null || !ref.current) return;
    pendingCaret.current = null;
    ref.current.focus();
    ref.current.setSelectionRange(at, at);
  }, [value]);

  const readCaret = () => setCaret(ref.current?.selectionStart ?? value.length);

  const choose = (item: Completion) => {
    if (ctx.kind === "none") return;
    const next = applyCompletion(value, ctx, item);
    onChange(next.text);
    setCaret(next.caret);
    setActive(-1);
    // Keep completing: after `sum:` comes a metric, after a metric a key.
    setOpen(true);
    pendingCaret.current = next.caret;
  };

  const onKeyDown = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (onSubmit && e.key === "Enter" && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      setOpen(false);
      onSubmit();
      return;
    }
    if (e.key === " " && e.ctrlKey) {
      e.preventDefault();
      setOpen(true);
      return;
    }
    if (!shown) return;
    if (e.key === "ArrowDown" || e.key === "ArrowUp") {
      if (items.length === 0) return;
      e.preventDefault();
      const step = e.key === "ArrowDown" ? 1 : -1;
      setActive((a) => (a + step + items.length) % items.length);
    } else if ((e.key === "Enter" || e.key === "Tab") && active >= 0 && items[active]) {
      e.preventDefault();
      choose(items[active]);
    } else if (e.key === "Escape") {
      e.preventDefault();
      setOpen(false);
    }
  };

  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-xs text-zinc-500">
        {label}
      </label>
      <div className="relative">
        <textarea
          id={id}
          ref={ref}
          role="combobox"
          aria-expanded={shown}
          aria-controls={listId}
          aria-autocomplete="list"
          aria-activedescendant={shown && active >= 0 ? `${listId}-${active}` : undefined}
          aria-invalid={verdict.kind === "invalid"}
          aria-describedby={`${id}-verdict`}
          spellCheck={false}
          rows={Math.min(4, value.split("\n").length)}
          value={value}
          onChange={(e) => {
            onChange(e.target.value);
            setCaret(e.target.selectionStart);
            setOpen(true);
            setActive(-1);
          }}
          onSelect={readCaret}
          onClick={readCaret}
          onFocus={() => setOpen(true)}
          onBlur={() => setOpen(false)}
          onKeyDown={onKeyDown}
          placeholder="sum:http.request.count{$env} by {route}.as_rate()"
          className="w-full resize-y rounded-md border border-zinc-300 bg-white px-2 py-1 font-mono text-xs outline-none focus:border-violet-500 aria-[invalid=true]:border-red-400 dark:border-zinc-700 dark:bg-zinc-900"
        />
        {shown ? <Suggestions id={listId} list={list} active={active} onChoose={choose} /> : null}
      </div>
      <Verdict id={`${id}-verdict`} text={value} verdict={verdict} onFormat={onChange} />
    </div>
  );
}

function Suggestions({
  id,
  list,
  active,
  onChoose,
}: {
  id: string;
  list: CompletionList;
  active: number;
  onChoose: (c: Completion) => void;
}) {
  const status = (text: string, tone = "text-zinc-500") => (
    <li role="presentation" className={`px-2 py-1 ${tone}`}>
      {text}
    </li>
  );
  return (
    <ul
      id={id}
      role="listbox"
      aria-label="Suggestions"
      className="absolute z-20 mt-1 max-h-56 w-full overflow-auto rounded-md border border-zinc-200 bg-white py-1 text-xs shadow-lg dark:border-zinc-700 dark:bg-zinc-900"
    >
      {list.status === "loading"
        ? status("Loading suggestions…")
        : list.status === "failed"
          ? status(list.message, "text-red-700 dark:text-red-400")
          : list.status === "blocked" || list.status === "empty"
            ? status(list.message)
            : list.status === "ready"
              ? list.items.map((item, i) => (
                  <li
                    key={`${item.detail}:${item.label}`}
                    id={`${id}-${i}`}
                    role="option"
                    aria-selected={i === active}
                    // mousedown, not click: it fires before the textarea's
                    // blur closes the list, and preventDefault keeps focus.
                    onMouseDown={(e) => {
                      e.preventDefault();
                      onChoose(item);
                    }}
                    className={`flex cursor-pointer justify-between gap-2 px-2 py-1 font-mono ${
                      i === active ? "bg-violet-100 dark:bg-violet-500/20" : "hover:bg-zinc-100 dark:hover:bg-zinc-800"
                    }`}
                  >
                    <span className="truncate">{item.label}</span>
                    {item.detail ? <span className="shrink-0 font-sans text-zinc-400">{item.detail}</span> : null}
                  </li>
                ))
              : null}
    </ul>
  );
}

/**
 * The line under the box: what ozyd said about this exact text. One branch
 * per [[ValidationState]], and the switch has no default so a new state is a
 * compile error here rather than a blank line.
 */
function Verdict({
  id,
  text,
  verdict,
  onFormat,
}: {
  id: string;
  text: string;
  verdict: ValidationState;
  onFormat: (q: string) => void;
}) {
  const line = (children: React.ReactNode, tone = "text-zinc-500") => (
    <div id={id} role="status" className={`text-xs ${tone}`}>
      {children}
    </div>
  );
  switch (verdict.kind) {
    case "blank":
      return line("Empty — this query is skipped until it says something, and refused on save.");
    case "checking":
      return line("Checking…", "text-zinc-400");
    case "ok":
      return line(
        <>
          <span className="text-emerald-700 dark:text-emerald-400">Parses.</span>
          {verdict.canonical !== text.trim() ? (
            <>
              {" "}
              <button type="button" onClick={() => onFormat(verdict.canonical)} className="underline">
                Format as <code className="font-mono">{verdict.canonical}</code>
              </button>
            </>
          ) : null}
        </>,
      );
    case "invalid":
      return line(<ParseError text={text} verdict={verdict} />, "text-red-700 dark:text-red-400");
    case "unrecognised":
      return line(
        "ozyd answered, but not in a form this build understands — the query has not been checked.",
        "text-amber-700 dark:text-amber-500",
      );
    case "failed":
      return line(`Could not check this query: ${verdict.message}`, "text-amber-700 dark:text-amber-500");
  }
}

/**
 * The text with the character ozyd named underlined. The underline is placed
 * by string index, not by ozyd's byte column — see [[byteColumnToIndex]] —
 * and is left off entirely when the column is not a character of this text.
 */
function ParseError({ text, verdict }: { text: string; verdict: Extract<ValidationState, { kind: "invalid" }> }) {
  if (verdict.index === undefined) {
    return (
      <p>
        {verdict.msg}{" "}
        <span className="text-zinc-500">
          (ozyd pointed at byte {verdict.col}, which is not a character of this text, so nothing is underlined)
        </span>
      </p>
    );
  }
  const at = verdict.index;
  const { line, column } = lineAndColumn(text, at);
  // One line of context: the line the error is on.
  const start = text.lastIndexOf("\n", at - 1) + 1;
  const endNl = text.indexOf("\n", at);
  const end = endNl === -1 ? text.length : endNl;
  const culprit = at < end ? String.fromCodePoint(text.codePointAt(at) as number) : "";
  return (
    <div>
      <p>
        {verdict.msg} <span className="text-zinc-500">(line {line}, column {column})</span>
      </p>
      <pre data-testid="parse-error" className="mt-0.5 overflow-x-auto font-mono text-zinc-700 dark:text-zinc-300">
        {text.slice(start, at)}
        <mark aria-label="error here" className="bg-transparent text-red-700 underline decoration-red-500 decoration-wavy dark:text-red-400">
          {/* The end of the query has no character to underline, so a
              placeholder stands in for "here, where something is missing". */}
          {culprit || "␣"}
        </mark>
        {text.slice(at + culprit.length, end)}
      </pre>
    </div>
  );
}
