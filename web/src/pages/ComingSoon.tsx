import { useLocation } from "react-router";
import { findNavItem } from "../lib/nav";
import { NotFound } from "./NotFound";

/** Placeholder for a section whose milestone hasn't landed yet. */
export function ComingSoon() {
  const item = findNavItem(useLocation().pathname);
  if (!item) return <NotFound />;
  return (
    <div className="max-w-xl">
      <h1 className="text-2xl font-semibold">{item.label}</h1>
      <p className="mt-2 text-zinc-600 dark:text-zinc-400">{item.description}</p>
      <p className="mt-4 inline-block rounded-md bg-zinc-100 px-2 py-1 text-sm dark:bg-zinc-800">
        Coming in {item.milestone}
      </p>
    </div>
  );
}
