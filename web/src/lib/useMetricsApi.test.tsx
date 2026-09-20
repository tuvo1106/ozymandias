import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import type { ReactNode } from "react";
import { DEFAULT_EXPLORER_STATE, type ExplorerState } from "./explorerState";
import {
  rangeKey,
  REFRESH_INTERVAL_MS,
  shouldAutoRefresh,
  useExplorerQuery,
  useTagKeys,
  useTagValues,
} from "./useMetricsApi";

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

const ok = (from = 0, to = 1) =>
  new Response(JSON.stringify({ status: "ok", from, to, interval: 10, series: [] }));

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
  const state: ExplorerState = { ...DEFAULT_EXPLORER_STATE, metric: "m", range: { kind: "relative", preset: "5m" } };

  it("stays idle without a metric", () => {
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
    const urls = f.mock.calls.map((c) => new URL((c as unknown as [string])[0], "http://x").searchParams);
    expect(urls.map((u) => [u.get("from"), u.get("to")])).toEqual([
      ["999700", "1000000"],
      ["999710", "1000010"],
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
});

describe("tag hooks", () => {
  it("stay idle until their inputs are known", () => {
    const f = vi.fn();
    vi.stubGlobal("fetch", f);
    renderHook(() => [useTagKeys(""), useTagValues("m", "")], { wrapper });
    expect(f).not.toHaveBeenCalled();
  });
});
