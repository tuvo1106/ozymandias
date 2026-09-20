import { useState } from "react";
import { Autocomplete } from "../../ui/Autocomplete";
import { useDebouncedValue } from "../../lib/useDebouncedValue";
import { useMetricNames } from "../../lib/useMetricsApi";

/** Props for MetricPicker. */
export interface MetricPickerProps {
  /** The metric currently in the URL. */
  metric: string;
  /** Called when the user commits a metric name. */
  onChange: (metric: string) => void;
}

/** Keystroke-to-request delay for metric-name suggestions. */
export const METRIC_DEBOUNCE_MS = 250;

/**
 * Metric-name autocomplete. The typed text is local until committed (pick a
 * suggestion or press Enter); only then does it reach the URL and trigger a
 * chart query. Parents remount it with `key={metric}` so a URL change (back
 * button, deep link) resets the text.
 */
export function MetricPicker({ metric, onChange }: MetricPickerProps) {
  const [text, setText] = useState(metric);
  const prefix = useDebouncedValue(text.trim(), METRIC_DEBOUNCE_MS);
  const names = useMetricNames(prefix);
  return (
    <Autocomplete
      label="Metric"
      placeholder="Search metrics, e.g. http.request.count"
      value={text}
      onChange={setText}
      options={names.data ?? []}
      loading={names.isFetching}
      onSubmit={(m) => {
        setText(m);
        if (m !== metric) onChange(m);
      }}
      onCancel={() => setText(metric)}
      className="w-full max-w-md"
    />
  );
}
