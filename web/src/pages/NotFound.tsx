import { Link } from "react-router";

/** Shown for any path the UI doesn't know. */
export function NotFound() {
  return (
    <div>
      <h1 className="text-2xl font-semibold">Page not found</h1>
      <p className="mt-2">
        <Link to="/" className="text-violet-600 underline dark:text-violet-400">
          Back to the overview
        </Link>
      </p>
    </div>
  );
}
