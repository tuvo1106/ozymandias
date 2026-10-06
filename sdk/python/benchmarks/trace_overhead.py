"""Tracer overhead per span: time and allocations, with tracing disabled / unsampled / sampled.

Run: ``uv run python benchmarks/trace_overhead.py`` (add ``--n 50000`` for steadier numbers).
No budget is set in advance (docs/plan/M5-tracing.md L12): the point is to measure, record the
number in the milestone notes, and make a regression visible.

What the three modes are:

* **disabled**: no agent host, the shipped default. ``trace()`` returns a shared no-op span.
* **enabled, unsampled**: ``OZY_TRACE_SAMPLE_RATE=0``. The span is still built, buffered and
  queued, because an unsampled trace is *still sent* (the agent needs 100% for statistics);
  only its ``_sampling_priority`` differs. So this should cost the same as sampled.
* **enabled, sampled**: rate 1.

The writer is replaced by one that only counts chunks: the benchmark measures the tracer,
not the loopback network, and a flusher thread competing for the GIL would add noise.
"""

from __future__ import annotations

import argparse
import statistics
import sys
import time
import tracemalloc
from typing import Any

import ozy
from ozy import Config, Tracer


class CountingWriter:
    """Stands in for ``TraceWriter``: accepts chunks and drops them."""

    chunks_sent = 0
    chunks_dropped = 0
    send_errors = 0

    def __init__(self) -> None:
        self.chunks = 0
        self.spans = 0

    def submit(self, chunk: list[dict[str, Any]]) -> None:
        self.chunks += 1
        self.spans += len(chunk)

    def flush(self) -> None:
        pass

    def close(self, budget: float = 0) -> None:
        pass


def make_tracer(mode: str) -> Tracer:
    tracer = Tracer()
    if mode == "disabled":
        return tracer
    rate = 0.0 if mode == "unsampled" else 1.0
    tracer.configure(
        Config(agent_host="127.0.0.1", trace_port=1, trace_sample_rate=rate, service="bench")
    )
    tracer._writer = CountingWriter()  # type: ignore[assignment]
    return tracer


def per_span_us(fn: Any, spans_per_call: int, n: int, repeats: int = 5) -> float:
    """Median over ``repeats`` of the mean microseconds per span."""
    fn(max(10, n // 50))  # warm up
    runs = []
    for _ in range(repeats):
        started = time.perf_counter_ns()
        fn(n)
        runs.append((time.perf_counter_ns() - started) / 1000 / (n * spans_per_call))
    return statistics.median(runs)


def bench_mode(mode: str, n: int) -> dict[str, float]:
    tracer = make_tracer(mode)

    def root_only(count: int) -> None:
        for _ in range(count):
            with tracer.trace("op"):
                pass

    def root_with_ten_children(count: int) -> None:
        for _ in range(count):
            with tracer.trace("root"):
                for _ in range(10):
                    with tracer.trace("child"):
                        pass

    @tracer.wrap("wrapped")
    def wrapped() -> None:
        pass

    def plain() -> None:
        pass

    def wrapped_calls(count: int) -> None:
        for _ in range(count):
            wrapped()

    def plain_calls(count: int) -> None:
        for _ in range(count):
            plain()

    results = {
        "root span (us)": per_span_us(root_only, 1, n),
        "child span in a 11-span trace (us)": per_span_us(
            root_with_ten_children, 11, max(n // 10, 10)
        ),
        "@wrap call overhead (us)": per_span_us(wrapped_calls, 1, n)
        - per_span_us(plain_calls, 1, n),
    }
    results.update(allocations(tracer))
    tracer.close()
    return results


def allocations(tracer: Tracer) -> dict[str, float]:
    """Peak transient bytes for one root span, and bytes retained per buffered child span."""
    tracemalloc.start()
    try:
        with tracer.trace("warm"):
            pass
        tracemalloc.reset_peak()
        base, _ = tracemalloc.get_traced_memory()
        with tracer.trace("op"):
            pass
        _, peak = tracemalloc.get_traced_memory()
        transient = max(0, peak - base)

        count = 200
        with tracer.trace("root"):
            before, _ = tracemalloc.get_traced_memory()
            for _ in range(count):
                tracer.start_span("child", activate=False).finish()
            after, _ = tracemalloc.get_traced_memory()
        retained = max(0, after - before) / count
    finally:
        tracemalloc.stop()
    return {
        "peak bytes, one root span": float(transient),
        "bytes retained per buffered span": retained,
    }


def run(n: int = 20000) -> dict[str, dict[str, float]]:
    """Measure every mode; returns ``{mode: {metric: value}}``."""
    return {mode: bench_mode(mode, n) for mode in ("disabled", "unsampled", "sampled")}


def main(argv: list[str] | None = None) -> int:
    """Print a table of the measurements."""
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0] if __doc__ else "")
    parser.add_argument("--n", type=int, default=20000, help="spans per timing run")
    args = parser.parse_args(argv)
    results = run(args.n)
    print(f"python {sys.version.split()[0]}, ozy {ozy.__version__}, n={args.n}")
    metrics = list(next(iter(results.values())))
    print(f"{'':42}" + "".join(f"{m:>12}" for m in results))
    for metric in metrics:
        print(f"{metric:42}" + "".join(f"{results[m][metric]:12.2f}" for m in results))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
