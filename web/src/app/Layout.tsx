import { NavLink, Outlet } from "react-router";
import { NAV_ITEMS } from "../lib/nav";

const linkClass = ({ isActive }: { isActive: boolean }) =>
  [
    "flex items-center justify-between rounded-md px-3 py-2 text-sm",
    isActive
      ? "bg-violet-100 font-medium text-violet-900 dark:bg-violet-500/15 dark:text-violet-200"
      : "text-zinc-600 hover:bg-zinc-100 hover:text-zinc-900 dark:text-zinc-400 dark:hover:bg-zinc-800 dark:hover:text-zinc-100",
  ].join(" ");

/**
 * The app shell: a sidebar with every product section (unbuilt ones show
 * which milestone brings them), and the routed page to its right.
 */
export function Layout() {
  return (
    <div className="flex min-h-screen bg-white text-zinc-900 dark:bg-zinc-950 dark:text-zinc-100">
      <aside className="w-56 shrink-0 border-r border-zinc-200 p-3 dark:border-zinc-800">
        <NavLink to="/" end className="mb-4 flex items-center gap-2 px-3 py-2 text-lg font-semibold">
          <span aria-hidden className="inline-block h-6 w-6 rounded-md bg-violet-600" />
          ozymandias
        </NavLink>
        <nav aria-label="Main" className="flex flex-col gap-0.5">
          {NAV_ITEMS.map((item) => (
            <NavLink key={item.path} to={item.path} className={linkClass}>
              <span>{item.label}</span>
              {!item.live && <span className="text-xs text-zinc-400 dark:text-zinc-500">{item.milestone}</span>}
            </NavLink>
          ))}
        </nav>
      </aside>
      <main className="min-w-0 flex-1 p-8">
        <Outlet />
      </main>
    </div>
  );
}
