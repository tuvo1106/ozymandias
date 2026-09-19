import { act, renderHook, waitFor } from "@testing-library/react";
import { useHealth } from "./useHealth";

const body = { status: "ok", component: "ozyd", version: "dev", uptime_seconds: 1 };

describe("useHealth", () => {
  it("goes from loading to ok, and polls on the interval", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const f = vi.fn(async () => new Response(JSON.stringify(body)));
    const { result, unmount } = renderHook(() => useHealth(1000, f as unknown as typeof fetch));
    expect(result.current.kind).toBe("loading");
    await waitFor(() => expect(result.current.kind).toBe("ok"));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });
    expect(f).toHaveBeenCalledTimes(2);
    unmount();
    vi.useRealTimers();
  });

  it("surfaces the error message", async () => {
    const f = vi.fn(async () => new Response("", { status: 500 }));
    const { result } = renderHook(() => useHealth(60_000, f as unknown as typeof fetch));
    await waitFor(() => expect(result.current).toEqual({ kind: "error", message: "ozyd answered 500" }));
  });

  it("ignores a response that arrives after unmount", async () => {
    let reject!: (e: unknown) => void;
    const f = vi.fn(() => new Promise<Response>((_, r) => (reject = r)));
    const { result, unmount } = renderHook(() => useHealth(60_000, f as unknown as typeof fetch));
    unmount();
    reject(new DOMException("aborted", "AbortError"));
    await Promise.resolve();
    expect(result.current.kind).toBe("loading");
  });
});
