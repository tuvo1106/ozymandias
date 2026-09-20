import { useState } from "react";
import { formatFilter, type TagFilter } from "../../lib/explorerState";
import { useTagKeys, useTagValues } from "../../lib/useMetricsApi";
import { Autocomplete } from "../../ui/Autocomplete";

/** Props for FilterBar. */
export interface FilterBarProps {
  metric: string;
  filters: TagFilter[];
  onChange: (filters: TagFilter[]) => void;
}

/** Suggestions containing the typed text, case-insensitively. */
function matching(options: readonly string[] | undefined, text: string): string[] {
  const t = text.trim().toLowerCase();
  return (options ?? []).filter((o) => o.toLowerCase().includes(t));
}

const chipClass =
  "inline-flex items-center gap-1 rounded-md border border-zinc-300 bg-zinc-50 px-2 py-0.5 text-sm dark:border-zinc-700 dark:bg-zinc-900";

/**
 * Tag filters as removable chips, plus a two-step adder: pick a tag key
 * (suggested from the metric's keys), then a value (suggested from that
 * key's values). Values may be typed freely, so `*` wildcards work; the
 * "=" / "≠" toggle, or typing the key as `!key`, makes a not-equal filter.
 * Value suggestions are filtered client-side because the tag-values endpoint
 * has no prefix parameter.
 */
export function FilterBar({ metric, filters, onChange }: FilterBarProps) {
  const [adding, setAdding] = useState(false);
  const [key, setKey] = useState("");
  const [keyText, setKeyText] = useState("");
  const [valueText, setValueText] = useState("");
  const [negate, setNegate] = useState(false);
  const keys = useTagKeys(adding ? metric : "");
  const values = useTagValues(metric, key);

  const reset = () => {
    setAdding(false);
    setKey("");
    setKeyText("");
    setValueText("");
    setNegate(false);
  };

  // A comma is the separator in both the statsd tag section and this page's
  // ?filter= parameter, and `wire.ValidTag` rejects tags containing one — so a
  // filter value with a comma can never match anything. It used to be accepted
  // silently and then split in the URL, leaving a chip that read `route:a`
  // while the user had typed `a,b`. Drop the character at the input instead.
  const noCommas = (s: string) => s.replace(/,/g, "");

  const add = (rawValue: string) => {
    const value = noCommas(rawValue);
    const f: TagFilter = { key, value, negate };
    if (!filters.some((x) => formatFilter(x) === formatFilter(f))) onChange([...filters, f]);
    reset();
  };

  return (
    <div className="flex flex-wrap items-center gap-2" aria-label="Filters" role="group">
      <span className="text-sm text-zinc-500">from</span>
      {filters.length === 0 && !adding && <span className={chipClass}>*</span>}
      {filters.map((f, i) => (
        <span key={formatFilter(f)} className={chipClass}>
          {formatFilter(f)}
          <button
            type="button"
            aria-label={`Remove filter ${formatFilter(f)}`}
            className="text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
            onClick={() => onChange(filters.filter((_, j) => j !== i))}
          >
            ×
          </button>
        </span>
      ))}
      {!adding && (
        <button
          type="button"
          disabled={!metric}
          onClick={() => setAdding(true)}
          className="rounded-md px-2 py-0.5 text-sm text-violet-700 hover:bg-violet-50 disabled:text-zinc-400 disabled:hover:bg-transparent dark:text-violet-300 dark:hover:bg-violet-500/10"
        >
          + Add filter
        </button>
      )}
      {adding && key === "" && (
        <Autocomplete
          label="Tag key"
          placeholder="tag key"
          autoFocus
          value={keyText}
          onChange={(k) => setKeyText(noCommas(k))}
          options={matching(keys.data, keyText.replace(/^!/, ""))}
          loading={keys.isFetching}
          onSubmit={(k) => {
            const clean = noCommas(k);
            const neg = clean.startsWith("!") || keyText.startsWith("!");
            setNegate(neg);
            setKey(clean.replace(/^!/, ""));
          }}
          onCancel={reset}
          className="w-40"
        />
      )}
      {adding && key !== "" && (
        <span className="inline-flex items-center gap-1">
          <span className="text-sm">{key}</span>
          <button
            type="button"
            aria-label={negate ? "Not equal (click for equal)" : "Equal (click for not equal)"}
            onClick={() => setNegate(!negate)}
            className="rounded border border-zinc-300 px-1 text-sm dark:border-zinc-700"
          >
            {negate ? "≠" : "="}
          </button>
          <Autocomplete
            label="Tag value"
            placeholder="value or pattern*"
            autoFocus
            value={valueText}
            onChange={(v) => setValueText(noCommas(v))}
            options={matching(values.data, valueText)}
            loading={values.isFetching}
            onSubmit={add}
            onCancel={reset}
            className="w-48"
          />
        </span>
      )}
      {adding && (
        <button
          type="button"
          aria-label="Cancel filter"
          onClick={reset}
          className="text-zinc-400 hover:text-zinc-900 dark:hover:text-zinc-100"
        >
          ×
        </button>
      )}
    </div>
  );
}
