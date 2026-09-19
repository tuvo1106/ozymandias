import type { RouteObject } from "react-router";
import { NAV_ITEMS } from "../lib/nav";
import { ComingSoon } from "../pages/ComingSoon";
import { Home } from "../pages/Home";
import { NotFound } from "../pages/NotFound";
import { Layout } from "./Layout";

/**
 * The route table. Each section gets its path and everything under it
 * (`/logs/*`), so deep links survive while a section is still a placeholder.
 * Kept separate from the router instance so tests can mount it in memory.
 */
export const routes: RouteObject[] = [
  {
    element: <Layout />,
    children: [
      { index: true, element: <Home /> },
      ...NAV_ITEMS.map((item) => ({ path: `${item.path.slice(1)}/*`, element: <ComingSoon /> })),
      { path: "*", element: <NotFound /> },
    ],
  },
];
