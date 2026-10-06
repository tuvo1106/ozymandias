import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";
import { TraceView } from "./TraceView";

const TID = "0123456789abcdef0123456789abcdef";
const span = (id: string, parent: string | null, name: string, service: string, start: number, dur: number, extra = {}) => ({
  trace_id: TID, span_id: id, parent_id: parent, service, name, resource: `${name} resource`, type: "web", start, duration: dur, error: 0, ...extra,
});
const detail = {
  trace_id: TID, services: ["api", "worker"], start: 1_000_000, duration: 100_000, span_count: 3, errors: 1, orphans: ["0000000000000003"],
  spans: [
    span("0000000000000001", null, "http.request", "api", 1_000_000, 100_000),
    span("0000000000000002", "0000000000000001", "postgres.query", "api", 1_010_000, 5_000, { error: 1, meta: { "error.type": "OperationalError" } }),
    span("0000000000000003", "00000000000000ff", "arq.job", "worker", 1_050_000, 20_000),
  ],
};

function renderView(fetchImpl: typeof fetch, path = `/apm/traces/${TID}`) {
  vi.stubGlobal("fetch", fetchImpl);
  return render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={[path]}>
        <Routes>
          <Route path="/apm/traces/:traceId" element={<TraceView />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

describe("TraceView", () => {
  it("summarizes the trace, names missing parents and shows the waterfall rows", async () => {
    renderView(vi.fn(async () => json(detail)) as unknown as typeof fetch);
    expect(await screen.findByText(/3 spans/)).toBeInTheDocument();
    expect(screen.getByText(/1 errors/)).toBeInTheDocument();
    expect(screen.getByText(/1 missing parent/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Waterfall" }));
    expect(screen.getByRole("button", { name: /postgres\.query/ })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /missing parent/ })).toBeInTheDocument();
  });

  it("shows a span's tags when it is selected, and the trace's logs on the Logs tab", async () => {
    const fetchMock = vi.fn(async (url: RequestInfo | URL) =>
      String(url).includes("/api/v1/logs")
        ? json({ logs: [{ ts: 1_000, message: "db failed", status: "error", service: "api" }], truncated: false })
        : json(detail),
    );
    renderView(fetchMock as unknown as typeof fetch, `/apm/traces/${TID}?span=0000000000000002`);
    expect(await screen.findByText("OperationalError")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Logs" }));
    expect(await screen.findByText("db failed")).toBeInTheDocument();
    expect(String(fetchMock.mock.calls.at(-1)?.[0])).toContain(`trace_id%3A${TID}`);
  });

  it("explains a trace that is not stored", async () => {
    renderView(vi.fn(async () => json({ error: "no spans stored for trace" }, 404)) as unknown as typeof fetch);
    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });
});
