/**
 * The product's navigation, and which milestone brings each section to life.
 * The shell renders every section from day one so the finished shape of the
 * UI is visible early (docs/plan/ui.md §2); sections whose milestone hasn't
 * landed render a "coming in M<n>" page instead of their content.
 */

/** One top-level section of the UI. */
export interface NavItem {
  /** Label shown in the sidebar. */
  label: string;
  /** Route path; every path under it belongs to this section. */
  path: string;
  /** Milestone that implements the section, e.g. "M3". */
  milestone: string;
  /** One sentence on what the section will do. */
  description: string;
}

/** Sidebar sections, in display order. */
export const NAV_ITEMS: readonly NavItem[] = [
  {
    label: "Metrics",
    path: "/metrics",
    milestone: "M1",
    description: "Explore any metric: pick a name, filter by tags, group, and chart it over time.",
  },
  {
    label: "Dashboards",
    path: "/dashboards",
    milestone: "M3",
    description: "Saved grids of widgets, provisioned from git, with template variables and a shared crosshair.",
  },
  {
    label: "Infrastructure",
    path: "/infrastructure",
    milestone: "M3",
    description: "Hosts and containers, sized by memory and coloured by CPU, grouped by compose project.",
  },
  {
    label: "Logs",
    path: "/logs",
    milestone: "M4",
    description: "Search, facet and live-tail logs from every app and container.",
  },
  {
    label: "APM",
    path: "/apm",
    milestone: "M5",
    description: "Services, traces, flame graphs and the service map — with logs one click from any span.",
  },
  {
    label: "Monitors",
    path: "/monitors",
    milestone: "M6",
    description: "Alert on any query, with hysteresis, no-data detection and notifications.",
  },
];

/**
 * Returns the section a pathname belongs to, or undefined for paths outside
 * every section. Matches whole path segments: "/logs/x" is in Logs,
 * "/logsearch" is not.
 */
export function findNavItem(pathname: string): NavItem | undefined {
  return NAV_ITEMS.find((item) => pathname === item.path || pathname.startsWith(item.path + "/"));
}
