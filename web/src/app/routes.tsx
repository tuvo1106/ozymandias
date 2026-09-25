import { Navigate, type RouteObject } from "react-router";
import { NAV_ITEMS } from "../lib/nav";
import { ComingSoon } from "../pages/ComingSoon";
import { Home } from "../pages/Home";
import { NotFound } from "../pages/NotFound";
import { Layout } from "./Layout";
import { Providers } from "./Providers";

/**
 * The route table. Sections that aren't built yet get their path and
 * everything under it (`/logs/*`), so deep links survive while a section is
 * still a placeholder; live sections list their real pages. Pages are
 * loaded lazily, so charting code (uPlot) is only downloaded by the pages
 * that draw charts. Kept separate from the router instance so tests can
 * mount it in memory.
 */
export const routes: RouteObject[] = [
  {
    element: (
      <Providers>
        <Layout />
      </Providers>
    ),
    // Shown while a lazily loaded page's first chunk downloads on a cold load.
    hydrateFallbackElement: <p className="p-8 text-zinc-500">Loading…</p>,
    children: [
      { index: true, element: <Home /> },
      {
        path: "metrics",
        children: [
          { index: true, element: <Navigate to="/metrics/explorer" replace /> },
          {
            path: "explorer",
            lazy: async () => ({ Component: (await import("../pages/metrics/MetricsExplorer")).MetricsExplorer }),
          },
          { path: "*", element: <NotFound /> },
        ],
      },
      ...NAV_ITEMS.filter((item) => !item.live).map((item) => ({
        path: `${item.path.slice(1)}/*`,
        element: <ComingSoon />,
      })),
      { path: "*", element: <NotFound /> },
    ],
  },
];
