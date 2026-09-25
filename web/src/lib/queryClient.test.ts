import { ApiError } from "./metricsApi";
import { createQueryClient, shouldRetry } from "./queryClient";

describe("shouldRetry", () => {
  it("never retries a client error", () => {
    expect(shouldRetry(0, new ApiError("bad filter", 400))).toBe(false);
    expect(shouldRetry(0, new ApiError("gone", 404))).toBe(false);
  });

  it("retries server and network errors twice", () => {
    for (const err of [new ApiError("boom", 500), new ApiError("unreachable"), new Error("other")]) {
      expect(shouldRetry(0, err)).toBe(true);
      expect(shouldRetry(1, err)).toBe(true);
      expect(shouldRetry(2, err)).toBe(false);
    }
  });
});

describe("createQueryClient", () => {
  it("installs the retry policy and disables focus refetching", () => {
    const opts = createQueryClient().getDefaultOptions().queries;
    expect(opts?.retry).toBe(shouldRetry);
    expect(opts?.refetchOnWindowFocus).toBe(false);
  });
});
