import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { DEFAULT_EXPLORER_STATE, type ExplorerState } from "./explorerState";
import {
  rangeKey,
  REFRESH_INTERVAL_MS,
  shouldAutoRefresh,
  useExplorerQuery,
} from "./useMetricsApi";

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const ok = (from = 0, to = 1) =>
  new Response(JSON.stringify({ status: "ok", query: "sum:m{*}", from, to, interval: 10, series: [], warnings: [] }));

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("rangeKey", () => {
  it("keys relative ranges by preset and absolute ones by bounds", () => {
    expect(rangeKey({ kind: "relative", preset: "4h" })).toBe("4h");
    expect(rangeKey({ kind: "absolute", from: 1, to: 2 })).toBe("1-2");
  });
});

describe("shouldAutoRefresh", () => {
  it("polls only live relative ranges", () => {
    const rel = { kind: "relative", preset: "1h" } as const;
    expect(shouldAutoRefresh({ live: true, range: rel })).toBe(true);
    expect(shouldAutoRefresh({ live: false, range: rel })).toBe(false);
    expect(shouldAutoRefresh({ live: true, range: { kind: "absolute", from: 1, to: 2 } })).toBe(false);
  });
});

describe("useExplorerQuery", () => {
  const state: ExplorerState = { ...DEFAULT_EXPLORER_STATE, q: "sum:m{*}", range: { kind: "relative", preset: "5m" } };

  it("stays idle without a query", () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    const { result } = renderHook(() => useExplorerQuery(DEFAULT_EXPLORER_STATE), { wrapper });
    expect(result.current.fetchStatus).toBe("idle");
    expect(f).not.toHaveBeenCalled();
  });

  it("re-resolves a relative range on every auto-refresh", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const f = vi.fn(async () => ok());
    vi.stubGlobal("fetch", f);
    let now = 1_000_000_000;
    const { result } = renderHook(() => useExplorerQuery(state, () => now), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    now += REFRESH_INTERVAL_MS;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(REFRESH_INTERVAL_MS);
    });
    await waitFor(() => expect(f).toHaveBeenCalledTimes(2));
    const bodies = f.mock.calls.map((c) => JSON.parse(String((c as unknown as [string, RequestInit])[1].body)) as { from: number; to: number });
    expect(bodies.map((b) => [b.from, b.to])).toEqual([
      [999_700, 1_000_000],
      [999_710, 1_000_010],
    ]);
  });

  it("does not poll when auto-refresh is off", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const f = vi.fn(async () => ok());
    vi.stubGlobal("fetch", f);
    const { result } = renderHook(() => useExplorerQuery({ ...state, live: false }), { wrapper });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(REFRESH_INTERVAL_MS * 3);
    });
    expect(f).toHaveBeenCalledTimes(1);
  });

  // The pairing the page relies on: an answer names the text it answers.
  it("says which query each answer is for, including the one kept while the next loads", async () => {
    let release: (() => void) | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn(async (_u: string, init: RequestInit) => {
        if ((JSON.parse(String(init.body)) as { q: string }).q === "sum:n{*}") await new Promise<void>((r) => (release = r));
        return ok();
      }),
    );
    const { result, rerender } = renderHook(({ s }) => useExplorerQuery(s), { wrapper, initialProps: { s: state } });
    await waitFor(() => expect(result.current.data?.asked).toBe("sum:m{*}"));
    rerender({ s: { ...state, q: "sum:n{*}" } });
    await waitFor(() => expect(result.current.isPlaceholderData).toBe(true));
    expect(result.current.data?.asked).toBe("sum:m{*}");
    await waitFor(() => expect(release).toBeDefined());
    release?.();
    await waitFor(() => expect(result.current.data?.asked).toBe("sum:n{*}"));
  });
});
