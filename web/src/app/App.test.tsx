import { render, screen } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { routes } from "./routes";

function renderAt(path: string) {
  return render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: [path] })} />);
}

afterEach(() => vi.unstubAllGlobals());

describe("app shell", () => {
  it("shows every section in the sidebar with its milestone", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    renderAt("/");
    const nav = screen.getByRole("navigation", { name: "Main" });
    for (const label of ["Metrics", "Dashboards", "Infrastructure", "Logs", "APM", "Monitors"]) {
      expect(nav).toHaveTextContent(label);
    }
    expect(screen.getByText("Checking ozyd…")).toBeInTheDocument();
  });

  it("shows ozyd's health on the home page", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () =>
        new Response(JSON.stringify({ status: "ok", component: "ozyd", version: "v0.1.0", uptime_seconds: 200 })),
      ),
    );
    renderAt("/");
    expect(await screen.findByText(/ozyd v0.1.0 is up — 3m 20s/)).toBeInTheDocument();
  });

  it("explains when ozyd is unreachable", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("Failed to fetch"))));
    renderAt("/");
    expect(await screen.findByRole("alert")).toHaveTextContent("ozyd is unreachable");
  });

  it("renders a placeholder naming the milestone for unbuilt sections, deep links included", () => {
    renderAt("/logs/live");
    expect(screen.getByRole("heading", { name: "Logs" })).toBeInTheDocument();
    expect(screen.getByText("Coming in M4")).toBeInTheDocument();
  });

  it("renders not-found for unknown paths", () => {
    renderAt("/nope");
    expect(screen.getByRole("heading", { name: "Page not found" })).toBeInTheDocument();
  });
});
