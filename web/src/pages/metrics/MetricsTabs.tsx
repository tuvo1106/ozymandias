import { NavLink } from "react-router";

const TABS = [
  { to: "/metrics/explorer", label: "Explorer" },
  { to: "/metrics/summary", label: "Summary" },
] as const;

/** The Metrics section's pages. */
export function MetricsTabs() {
  return (
    <nav aria-label="Metrics pages" className="flex gap-1 border-b border-zinc-200 text-sm dark:border-zinc-800">
      {TABS.map((t) => (
        <NavLink
          key={t.to}
          to={t.to}
          className={({ isActive }) =>
            `-mb-px border-b-2 px-3 py-1.5 ${
              isActive ? "border-violet-600 font-medium" : "border-transparent text-zinc-500 hover:text-zinc-800 dark:hover:text-zinc-200"
            }`
          }
        >
          {t.label}
        </NavLink>
      ))}
    </nav>
  );
}
