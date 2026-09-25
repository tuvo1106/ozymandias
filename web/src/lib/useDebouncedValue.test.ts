import { act, renderHook } from "@testing-library/react";
import { useDebouncedValue } from "./useDebouncedValue";

describe("useDebouncedValue", () => {
  afterEach(() => vi.useRealTimers());

  it("only settles after the value stops changing", () => {
    vi.useFakeTimers();
    const { result, rerender } = renderHook(({ v }) => useDebouncedValue(v, 100), { initialProps: { v: "a" } });
    expect(result.current).toBe("a");
    rerender({ v: "ab" });
    act(() => vi.advanceTimersByTime(60));
    rerender({ v: "abc" });
    act(() => vi.advanceTimersByTime(60));
    expect(result.current).toBe("a");
    act(() => vi.advanceTimersByTime(40));
    expect(result.current).toBe("abc");
  });
});
