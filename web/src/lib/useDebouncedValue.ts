import { useEffect, useState } from "react";

/**
 * Returns `value`, but only after it has stopped changing for `delayMs`.
 * Autocomplete inputs key their server queries on the debounced value so
 * typing "http.request" costs one request, not twelve.
 */
export function useDebouncedValue<T>(value: T, delayMs: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(id);
  }, [value, delayMs]);
  return debounced;
}
