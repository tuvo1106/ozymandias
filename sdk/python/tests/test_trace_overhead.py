"""The overhead benchmark runs, and measures what it says it measures."""

from __future__ import annotations

import importlib.util
import sys
from pathlib import Path
from types import ModuleType

import pytest

BENCH = Path(__file__).resolve().parents[1] / "benchmarks" / "trace_overhead.py"


@pytest.fixture(scope="module")
def bench() -> ModuleType:
    spec = importlib.util.spec_from_file_location("trace_overhead", BENCH)
    assert spec is not None
    assert spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    sys.modules["trace_overhead"] = module
    spec.loader.exec_module(module)
    return module


def test_every_mode_reports_every_metric(bench: ModuleType) -> None:
    results = bench.run(300)
    assert set(results) == {"disabled", "unsampled", "sampled"}
    for metrics in results.values():
        assert len(metrics) == 5
        assert all(value >= 0 for key, value in metrics.items() if "overhead" not in key)


def test_disabled_is_cheaper_than_enabled_and_unsampled_costs_the_same_as_sampled(
    bench: ModuleType,
) -> None:
    results = bench.run(2000)
    assert results["disabled"]["root span (us)"] < results["sampled"]["root span (us)"]
    assert results["disabled"]["bytes retained per buffered span"] == 0
    assert results["sampled"]["bytes retained per buffered span"] > 0
    # Unsampled traces are still built, buffered and sent (the agent needs them for stats).
    ratio = results["unsampled"]["root span (us)"] / results["sampled"]["root span (us)"]
    assert 0.3 < ratio < 3


def test_the_unsampled_mode_really_sends_priority_zero_traces(bench: ModuleType) -> None:
    tracer = bench.make_tracer("unsampled")
    with tracer.trace("op"):
        pass
    writer = tracer._writer
    assert writer.chunks == 1
    assert bench.make_tracer("disabled").enabled is False


def test_main_prints_a_table(bench: ModuleType, capsys: pytest.CaptureFixture[str]) -> None:
    assert bench.main(["--n", "200"]) == 0
    out = capsys.readouterr().out
    assert "root span (us)" in out
    assert "disabled" in out
