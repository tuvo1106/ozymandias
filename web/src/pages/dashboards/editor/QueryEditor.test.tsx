import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { QueryEditor } from "./QueryEditor";

type Handler = (path: string, params: URLSearchParams, body: Record<string, unknown> | undefined) => {
  status?: number;
  body: unknown;
} | "hang";

function mockApi(handler: Handler) {
  const f = vi.fn(async (input: string, init?: RequestInit) => {
    const url = new URL(input, "http://localhost");
    const body = init?.body ? (JSON.parse(String(init.body)) as Record<string, unknown>) : undefined;
    const reply = handler(url.pathname, url.searchParams, body);
    if (reply === "hang") return new Promise<Response>(() => {});
    return new Response(JSON.stringify(reply.body), { status: reply.status ?? 200 });
  });
  vi.stubGlobal("fetch", f);
  return f;
}

/** Parses whatever does not contain "!!"; errors point at the first "!". */
const validator: Handler = (path, _p, body) => {
  if (path !== "/api/v1/query/validate") return { status: 404, body: {} };
  const q = String(body?.q ?? "");
  const bang = new TextEncoder().encode(q.slice(0, Math.max(q.indexOf("!!"), 0))).length;
  return q.includes("!!")
    ? { body: { ok: false, error: { msg: "unexpected '!'", col: bang + 1 } } }
    : { body: { ok: true, query: q.trim().toLowerCase() } };
};

function Harness({ initial = "", variables = [] as string[] }) {
  const [q, setQ] = useState(initial);
  return <QueryEditor label="Query" value={q} onChange={setQ} variables={variables} />;
}

function renderEditor(props: Parameters<typeof Harness>[0] = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <Harness {...props} />
    </QueryClientProvider>,
  );
  return screen.getByRole("combobox", { name: "Query" }) as HTMLTextAreaElement;
}

afterEach(() => vi.unstubAllGlobals());

describe("QueryEditor verdicts", () => {
  it("underlines the character ozyd names, counted in characters not bytes", async () => {
    mockApi(validator);
    renderEditor({ initial: "sum:m{a:café!!}" });
    const mark = await screen.findByLabelText("error here");
    // Byte column 14 (é is two bytes) is string index 12, the first "!".
    expect(mark).toHaveTextContent("!");
    expect(screen.getByTestId("parse-error")).toHaveTextContent("sum:m{a:café!!}");
    expect(screen.getByText(/line 1, column 13/)).toBeInTheDocument();
  });

  it("drops the underline the moment the text changes, until the new text is answered", async () => {
    const f = mockApi(validator);
    const box = renderEditor({ initial: "sum:x{!!}" });
    await screen.findByLabelText("error here");
    f.mockImplementation(async () => new Promise<Response>(() => {})); // the next answer never comes
    await userEvent.type(box, "z");
    expect(screen.queryByLabelText("error here")).not.toBeInTheDocument();
    expect(screen.getByText("Checking…")).toBeInTheDocument();
  });

  it("offers the canonical spelling as a format button", async () => {
    mockApi(validator);
    renderEditor({ initial: "SUM:x{*}" });
    await userEvent.click(await screen.findByRole("button", { name: /Format as/ }));
    expect(screen.getByRole("combobox")).toHaveValue("sum:x{*}");
    expect(await screen.findByText("Parses.")).toBeInTheDocument();
  });

  it("does not call an unchecked query valid or invalid", async () => {
    mockApi(() => ({ status: 503, body: { error: "store down" } }));
    renderEditor({ initial: "sum:x{*}" });
    expect(await screen.findByText("Could not check this query: store down")).toBeInTheDocument();
    expect(screen.queryByText("Parses.")).not.toBeInTheDocument();
  });

  it("says when ozyd's answer is not one it can read", async () => {
    mockApi(() => ({ body: { ok: true } }));
    renderEditor({ initial: "sum:x{*}" });
    expect(await screen.findByText(/not in a form this build understands/)).toBeInTheDocument();
  });

  it("does not check a blank query, and says what blank means", () => {
    const f = mockApi(validator);
    renderEditor({ initial: "" });
    expect(screen.getByText(/Empty — this query is skipped/)).toBeInTheDocument();
    expect(f).not.toHaveBeenCalled();
  });

  it("says when the column is not a character of the text", async () => {
    mockApi(() => ({ body: { ok: false, error: { msg: "odd", col: 99 } } }));
    renderEditor({ initial: "sum:x" });
    expect(await screen.findByText(/pointed at byte 99, which is not a character/)).toBeInTheDocument();
    expect(screen.queryByLabelText("error here")).not.toBeInTheDocument();
  });
});

describe("QueryEditor completions", () => {
  it("completes an aggregator, then a metric from the store, then a key", async () => {
    mockApi((path, params, body) => {
      if (path === "/api/v1/metrics") return { body: { metrics: ["http.request.count", "http.other"].filter((m) => m.startsWith(params.get("prefix") ?? "")) } };
      if (path === "/api/v1/tags") return { body: { keys: ["route", "service"] } };
      return validator(path, params, body);
    });
    const box = renderEditor();
    await userEvent.type(box, "su");
    await userEvent.click(within(screen.getByRole("listbox")).getByRole("option", { name: /sum/ }));
    expect(box).toHaveValue("sum:");
    await userEvent.type(box, "http.r");
    await userEvent.click(await screen.findByRole("option", { name: /http\.request\.count/ }));
    expect(box).toHaveValue("sum:http.request.count{");
    await userEvent.click(await screen.findByRole("option", { name: /service/ }));
    expect(box).toHaveValue("sum:http.request.count{service:");
  });

  it("shows loading while the store is asked, not an empty list", async () => {
    mockApi((path, p, b) => (path === "/api/v1/metrics" ? "hang" : validator(path, p, b)));
    const box = renderEditor();
    await userEvent.type(box, "sum:ht");
    expect(await screen.findByText("Loading suggestions…")).toBeInTheDocument();
    expect(screen.queryByText(/No metric starts/)).not.toBeInTheDocument();
  });

  // The answer for "h" is capped at SUGGESTION_LIMIT names, so filtering it
  // for "hz" can miss a metric the store has. Until "hz" itself is answered
  // the list is loading, never "no metric starts with hz".
  it("does not answer a longer prefix from a shorter prefix's capped list", async () => {
    mockApi((path, params, body) => {
      if (path === "/api/v1/metrics") return { body: { metrics: params.get("prefix") === "hz" ? ["hzz"] : ["h1"] } };
      return validator(path, params, body);
    });
    const box = renderEditor();
    await userEvent.type(box, "sum:h");
    await screen.findByRole("option", { name: /h1/ });
    await userEvent.type(box, "z");
    expect(screen.queryByText(/No metric starts/)).not.toBeInTheDocument();
    expect(await screen.findByRole("option", { name: /hzz/ })).toBeInTheDocument();
  });

  it("says suggestions failed rather than that nothing matches", async () => {
    mockApi((path, p, b) => (path === "/api/v1/tags" ? { status: 500, body: { error: "boom" } } : validator(path, p, b)));
    const box = renderEditor();
    await userEvent.type(box, "sum:m{{");
    expect(await screen.findByText("Suggestions are unavailable: boom")).toBeInTheDocument();
  });

  it("falls back to the .count series for a distribution's tags", async () => {
    const f = mockApi((path, params, body) => {
      if (path === "/api/v1/tags")
        return { body: { keys: params.get("metric") === "lat.count" ? ["route"] : [] } };
      return validator(path, params, body);
    });
    const box = renderEditor();
    await userEvent.type(box, "p95:lat{{");
    expect(await screen.findByRole("option", { name: /route/ })).toBeInTheDocument();
    await waitFor(() =>
      expect(f.mock.calls.map(([u]) => new URL(u, "http://x").searchParams.get("metric"))).toContain("lat.count"),
    );
  });

  it("completes template variables from the dashboard, with keyboard", async () => {
    mockApi(validator);
    const box = renderEditor({ variables: ["env", "region"] });
    await userEvent.type(box, "sum:m{{$r");
    await userEvent.keyboard("{ArrowDown}{Enter}");
    expect(box).toHaveValue("sum:m{$region");
  });
});
