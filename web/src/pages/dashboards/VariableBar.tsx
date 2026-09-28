/**
 * The row of selectors above a dashboard, one per `template_vars` entry.
 *
 * Each offers the values its tag actually takes, asked of the metric the
 * dashboard queries — so the bar shows what exists rather than what somebody
 * typed into the definition. The list comes from `/api/v1/tags/values`, which
 * wants a metric, so the bar is told which one to ask about; a dashboard whose
 * widgets query several metrics uses the first, because a union of every
 * metric's values would offer combinations that select nothing.
 *
 * "All" is always first and is what an empty value means: no filter, rather
 * than a filter that happens to match everything. That distinction is the
 * evaluator's (an unbound variable is an error, a variable bound to nothing is
 * a warning), and the bar has to keep it visible.
 */
import type { TemplateVar } from "../../lib/dashboard";
import { selectedValue, type DashboardViewState } from "../../lib/dashboardState";
import { useVariableValues } from "../../lib/useDashboards";

/** Props for one selector. */
interface VariableSelectProps {
  variable: TemplateVar;
  metric: string;
  value: string;
  onChange: (value: string) => void;
}

function VariableSelect({ variable, metric, value, onChange }: VariableSelectProps) {
  const values = useVariableValues(metric, variable.tag);
  // The chosen value is offered even when the list has not arrived or no
  // longer contains it: a link pinned to a service that stopped reporting
  // should still show which service it was pinned to, not silently widen to
  // every service.
  const options = [...new Set([...values, ...(value ? [value] : [])])].sort();
  return (
    <label className="flex items-center gap-1.5 text-sm">
      <span className="text-zinc-500">${variable.name}</span>
      <select
        aria-label={`${variable.name} (${variable.tag})`}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        className="rounded-md border border-zinc-300 bg-white px-2 py-1 text-sm dark:border-zinc-700 dark:bg-zinc-900"
      >
        <option value="">All</option>
        {options.map((v) => (
          <option key={v} value={v}>
            {v}
          </option>
        ))}
      </select>
    </label>
  );
}

/** Props for VariableBar. */
export interface VariableBarProps {
  variables: readonly TemplateVar[];
  state: DashboardViewState;
  /** The metric whose tag values the selectors offer. */
  metric: string;
  onChange: (name: string, value: string) => void;
}

/** Draws one selector per declared variable, or nothing if there are none. */
export function VariableBar({ variables, state, metric, onChange }: VariableBarProps) {
  if (variables.length === 0) return null;
  return (
    <div aria-label="Template variables" className="flex flex-wrap items-center gap-3">
      {variables.map((variable) => (
        <VariableSelect
          key={variable.name}
          variable={variable}
          metric={metric}
          value={selectedValue(variable, state)}
          onChange={(value) => onChange(variable.name.toLowerCase(), value)}
        />
      ))}
    </div>
  );
}
