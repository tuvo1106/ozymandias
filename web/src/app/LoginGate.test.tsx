import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ApiError } from "../lib/metricsApi";
import { createQueryClient } from "../lib/queryClient";
import { notifyIfLoggedOut, UNAUTHORIZED_EVENT } from "../lib/authApi";
import { LoginGate } from "./LoginGate";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });

afterEach(() => {
  vi.unstubAllGlobals();
  cleanup();
});

describe("LoginGate", () => {
  it("shows the app at once, and keeps showing it when ozyd has no auth", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ status: "error" }, 404)));
    render(<LoginGate>the app</LoginGate>);
    expect(screen.getByText("the app")).toBeInTheDocument();
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.queryByLabelText("Admin password")).toBeNull();
  });

  it("shows the app while the probe has not answered", () => {
    vi.stubGlobal("fetch", vi.fn(() => new Promise(() => {})));
    render(<LoginGate>the app</LoginGate>);
    expect(screen.getByText("the app")).toBeInTheDocument();
  });

  it("shows the form when auth is on and this browser is not logged in", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ enabled: true, authenticated: false })));
    render(<LoginGate>the app</LoginGate>);
    expect(await screen.findByLabelText("Admin password")).toBeInTheDocument();
    expect(screen.queryByText("the app")).toBeNull();
  });

  it("keeps the app when auth is on and the session is good", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ enabled: true, authenticated: true })));
    render(<LoginGate>the app</LoginGate>);
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.getByText("the app")).toBeInTheDocument();
  });

  it("does not act on a session-shaped body from a failed response", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ enabled: true, authenticated: false }, 502)));
    render(<LoginGate>the app</LoginGate>);
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.getByText("the app")).toBeInTheDocument();
    expect(screen.queryByLabelText("Admin password")).toBeNull();
  });

  it("does not trust a body that is not a session", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ status: "ok", component: "ozyd" })));
    render(<LoginGate>the app</LoginGate>);
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    expect(screen.getByText("the app")).toBeInTheDocument();
  });

  it("swaps to the form when a call is answered 401", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => json({ enabled: true, authenticated: true })));
    render(<LoginGate>the app</LoginGate>);
    await waitFor(() => expect(fetch).toHaveBeenCalled());
    window.dispatchEvent(new Event(UNAUTHORIZED_EVENT));
    expect(await screen.findByLabelText("Admin password")).toBeInTheDocument();
  });

  it("says why a login failed", async () => {
    const f = vi.fn<(url: string, init?: RequestInit) => Promise<Response>>(async (url) =>
      url.endsWith("/session") ? json({ enabled: true, authenticated: false }) : json({ status: "error" }, 401),
    );
    vi.stubGlobal("fetch", f);
    render(<LoginGate>the app</LoginGate>);
    await userEvent.type(await screen.findByLabelText("Admin password"), "nope");
    await userEvent.click(screen.getByRole("button", { name: "Log in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Wrong password");
    const call = f.mock.calls.find((c) => c[0].endsWith("/login"))!;
    expect(JSON.parse(String(call[1]?.body))).toEqual({ password: "nope" });
  });
});

describe("query client", () => {
  it("tells the gate when a query is answered 401, and only then", async () => {
    const seen = vi.fn();
    window.addEventListener(UNAUTHORIZED_EVENT, seen);
    const client = createQueryClient();
    const run = async (err: unknown) => {
      await client.fetchQuery({ queryKey: [String(Math.random())], queryFn: () => Promise.reject(err), retry: false }).catch(() => {});
    };
    await run(new ApiError("nope", 404));
    await run(new Error("network"));
    expect(seen).not.toHaveBeenCalled();
    await run(new ApiError("login required", 401));
    expect(seen).toHaveBeenCalledTimes(1);
    window.removeEventListener(UNAUTHORIZED_EVENT, seen);
  });
});

describe("401 from a mutation, and from a cut-off stream", () => {
  it("a mutation answered 401 tells the gate, as a query does", async () => {
    const seen = vi.fn();
    window.addEventListener(UNAUTHORIZED_EVENT, seen);
    const client = createQueryClient();
    await client.getMutationCache().build(client, { mutationFn: () => Promise.reject(new ApiError("login required", 401)) }).execute(undefined).catch(() => {});
    expect(seen).toHaveBeenCalledTimes(1);
    await client.getMutationCache().build(client, { mutationFn: () => Promise.reject(new ApiError("bad", 400)) }).execute(undefined).catch(() => {});
    expect(seen).toHaveBeenCalledTimes(1);
    window.removeEventListener(UNAUTHORIZED_EVENT, seen);
  });

  it("notifyIfLoggedOut tells the gate only when auth is on and the session is gone", async () => {
    const seen = vi.fn();
    window.addEventListener(UNAUTHORIZED_EVENT, seen);
    for (const [body, want] of [
      [{ enabled: true, authenticated: true }, 0],
      [{ enabled: false, authenticated: false }, 0],
      [{ enabled: true, authenticated: false }, 1],
    ] as const) {
      seen.mockClear();
      vi.stubGlobal("fetch", vi.fn(async () => json(body)));
      await notifyIfLoggedOut();
      expect(seen).toHaveBeenCalledTimes(want);
    }
    vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("offline"))));
    seen.mockClear();
    await notifyIfLoggedOut();
    expect(seen).not.toHaveBeenCalled();
    window.removeEventListener(UNAUTHORIZED_EVENT, seen);
  });
});
