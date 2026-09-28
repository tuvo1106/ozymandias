import { render, screen, within } from "@testing-library/react";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { routes } from "../../app/routes";

const row = (id: number, title: string, extra: Record<string, unknown> = {}) => ({
  id,
  provisioned: false,
  created_at: "t",
  updated_at: "t",
  title,
  widgets: [{ id: "w", type: "note", layout: { x: 0, y: 0, w: 12, h: 1 }, markdown: "hi" }],
  ...extra,
});

interface Reply {
  status?: number;
  body: unknown;
}

function mockApi(replies: Record<string, Reply>) {
  const f = vi.fn(async (input: string) => {
    const { pathname } = new URL(input, "http://localhost");
    const reply = replies[pathname] ?? { status: 404, body: { error: `no route for ${pathname}` } };
    return new Response(JSON.stringify(reply.body), { status: reply.status ?? 200 });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

function renderIndex(replies: Record<string, Reply>) {
  mockApi(replies);
  render(<RouterProvider router={createMemoryRouter(routes, { initialEntries: ["/dashboards"] })} />);
}

afterEach(() => vi.unstubAllGlobals());

describe("DashboardsIndex", () => {
  it("lists stored dashboards and the services a template covers, separately", async () => {
    renderIndex({
      "/api/v1/dashboards": {
        body: {
          dashboards: [
            row(1, "Checkout", { description: "the money path", provisioned: true }),
            row(2, "Service overview", { template: true, uid: "service-overview" }),
          ],
          unreadable: [],
        },
      },
      "/api/v1/dashboards/services": { body: { services: ["api", "worker"], truncated: false } },
    });

    // The link first: the two panels render before either request lands, so
    // finding the region proves nothing about the list inside it.
    expect(await screen.findByRole("link", { name: /Checkout/ })).toHaveAttribute("href", "/dashboards/1");
    const saved = screen.getByRole("region", { name: "Saved" });
    expect(within(saved).getByText("from file")).toBeInTheDocument();
    // A template has no data of its own; it is offered through its services.
    expect(within(saved).queryByText("Service overview")).not.toBeInTheDocument();

    const svc = screen.getByRole("region", { name: "Services" });
    expect(await within(svc).findByRole("link", { name: "api" })).toHaveAttribute("href", "/dashboards/service/api");
    expect(within(svc).getByRole("link", { name: "worker" })).toBeInTheDocument();
  });

  it("escapes a service name that would otherwise be a path", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [], unreadable: [] } },
      "/api/v1/dashboards/services": { body: { services: ["a/b"], truncated: false } },
    });
    expect(await screen.findByRole("link", { name: "a/b" })).toHaveAttribute("href", "/dashboards/service/a%2Fb");
  });

  it("names the rows it could not read, so they do not look deleted", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [row(1, "Checkout")], unreadable: [4, 9] } },
      "/api/v1/dashboards/services": { body: { services: [], truncated: false } },
    });
    expect(await screen.findByText(/2 stored dashboard\(s\) could not be read \(ids 4, 9\)/)).toBeInTheDocument();
  });

  // A template is filtered out of the list, so a deployment provisioning only
  // templates has rows and draws none of them — and a panel that is blank
  // without saying why is what the message is for.
  it("says None yet when every stored dashboard is a template", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [row(2, "Service overview", { template: true })], unreadable: [] } },
      "/api/v1/dashboards/services": { body: { services: ["api"], truncated: false } },
    });
    expect(await screen.findByText(/None yet/)).toBeInTheDocument();
  });

  it("says a partial service list is partial", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [], unreadable: [] } },
      "/api/v1/dashboards/services": { body: { services: ["api"], truncated: true } },
    });
    expect(await screen.findByText(/This list is partial/)).toBeInTheDocument();
  });

  // Discovery needs a metric store; dashboard CRUD does not. A server without
  // one answers 503 here and still serves the list above it.
  it("keeps the saved list when service discovery is unavailable", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [row(1, "Checkout")], unreadable: [] } },
      "/api/v1/dashboards/services": { status: 503, body: { error: "this server has no metric store" } },
    });
    expect(await screen.findByRole("link", { name: /Checkout/ })).toBeInTheDocument();
    // A generous timeout: a 5xx is retried twice with backoff before the error
    // is shown, because unlike a 4xx it is the kind of failure that passes.
    expect(await screen.findByRole("alert", {}, { timeout: 10_000 })).toHaveTextContent("this server has no metric store");
  });

  it("explains an empty deployment instead of showing two empty lists", async () => {
    renderIndex({
      "/api/v1/dashboards": { body: { dashboards: [], unreadable: [] } },
      "/api/v1/dashboards/services": { body: { services: [], truncated: false } },
    });
    expect(await screen.findByText(/None yet/)).toBeInTheDocument();
    expect(await screen.findByText(/No service is reporting a metric any template queries yet/)).toBeInTheDocument();
  });
});
