"""The tracer core: span lifecycle, context, per-trace buffers, sampling, wire limits."""

from __future__ import annotations

import asyncio
import concurrent.futures
import contextlib
import dataclasses
import json
import math
import re
import threading
from typing import Any

import pytest

import ozy
from ozy import Config, Context, Tracer

from .conftest import FakeTraceAgent, TracerFactory

HEX32 = re.compile(r"[0-9a-f]{32}")
HEX16 = re.compile(r"[0-9a-f]{16}")


def sent(agent: FakeTraceAgent, tracer: Tracer) -> list[dict[str, Any]]:
    tracer.flush()
    return agent.spans()


def by_name(spans: list[dict[str, Any]]) -> dict[str, dict[str, Any]]:
    return {s["name"]: s for s in spans}


# -- lifecycle ---------------------------------------------------------------------


def test_span_has_wire_shaped_ids_and_defaults(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(version="1.2.3")
    with tracer.trace("op"):
        pass
    (span,) = sent(trace_agent, tracer)
    assert HEX32.fullmatch(span["trace_id"])
    assert HEX16.fullmatch(span["span_id"])
    assert span["parent_id"] is None
    assert span["service"] == "svc"
    assert span["resource"] == "op"  # defaults to the name
    assert span["type"] == "custom"
    assert span["error"] == 0
    assert span["meta"]["env"] == "test"
    assert span["meta"]["version"] == "1.2.3"
    assert span["metrics"]["_top_level"] == 1
    assert span["metrics"]["_sampling_priority"] == 1


def test_ids_are_never_zero(make_tracer: TracerFactory) -> None:
    # An all-zero draw is invalid on the wire, so it is redrawn rather than sent.
    draws = iter([bytes(16), b"\x00" * 15 + b"\x07", bytes(8), bytes(8), b"\x00" * 7 + b"\x01"])
    tracer = make_tracer(random_bytes=lambda n: next(draws))
    with tracer.trace("op") as span:
        assert span.trace_id == "0" * 30 + "07"
        assert span.span_id == "0" * 15 + "1"


def test_timing_uses_wall_clock_for_start_and_monotonic_for_duration(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    wall = iter([1_790_000_000_123_456_789] * 10)
    mono = iter([1_000_000, 4_500_000, 4_500_000, 4_500_000])
    tracer = make_tracer(wall_ns=lambda: next(wall), mono_ns=lambda: next(mono))
    with tracer.trace("op"):
        pass
    (span,) = sent(trace_agent, tracer)
    assert span["start"] == 1_790_000_000_123_456  # microseconds, from the wall clock
    assert span["duration"] == 3_500  # 3.5 ms in microseconds, from the monotonic clock


def test_a_clock_that_goes_backwards_never_yields_a_negative_duration(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    mono = iter([9_000_000, 1_000_000])
    tracer = make_tracer(mono_ns=lambda: next(mono))
    with tracer.trace("op"):
        pass
    (span,) = sent(trace_agent, tracer)
    assert span["duration"] == 0


def test_finish_is_idempotent(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()
    span = tracer.start_span("op")
    span.finish()
    duration = span.duration_us
    span.set_tag("late", "x")  # ignored: the span has already been recorded
    span.set_metric("late", 1)
    span.set_error(RuntimeError("late"))
    span.finish()
    assert span.duration_us == duration
    (wire,) = sent(trace_agent, tracer)
    assert "late" not in wire["meta"]
    assert wire["error"] == 0
    assert tracer.stats().spans_finished == 1


def test_exception_marks_the_span_and_reraises_the_same_object(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    boom = ValueError("bad input")
    with pytest.raises(ValueError, match="bad input") as caught, tracer.trace("op"):
        raise boom
    assert caught.value is boom  # not wrapped, not replaced
    (span,) = sent(trace_agent, tracer)
    assert span["error"] == 1
    assert span["meta"]["error.type"] == "ValueError"
    assert span["meta"]["error.message"] == "bad input"
    assert "ValueError: bad input" in span["meta"]["error.stack"]
    assert "test_tracer.py" in span["meta"]["error.stack"]


def test_custom_exception_type_is_module_qualified(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    class Custom(Exception):
        pass

    tracer = make_tracer()
    with contextlib.suppress(Custom), tracer.trace("op"):
        raise Custom("x")
    (span,) = sent(trace_agent, tracer)
    assert span["meta"]["error.type"].endswith("Custom")
    assert "." in span["meta"]["error.type"]


def test_an_exception_with_a_broken_str_is_still_recorded(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    class Evil(Exception):
        def __str__(self) -> str:
            raise RuntimeError("no str for you")

    tracer = make_tracer()
    with pytest.raises(Evil), tracer.trace("op"):
        raise Evil()
    # Describing the exception failed inside the SDK; that must neither raise nor lose the span.
    (span,) = sent(trace_agent, tracer)
    assert span["error"] == 1
    assert span["meta"]["error.message"] == "<unprintable Evil>"


def test_cancellation_is_not_an_error(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    async def run() -> None:
        async def work() -> None:
            async with tracer.trace("cancelled"):
                await asyncio.sleep(10)

        task = asyncio.create_task(work())
        await asyncio.sleep(0.01)
        task.cancel()
        with contextlib.suppress(asyncio.CancelledError):
            await task

    asyncio.run(run())
    (span,) = sent(trace_agent, tracer)
    assert span["error"] == 0


def test_set_tag_and_metric_value_handling(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("op", tags={"a": 1}) as span:
        span.set_tag("flag", True)
        span.set_tag("off", False)
        span.set_tag("none", None)
        span.set_tag("obj", object)
        span.set_metric("n", 3)
        span.set_metric("nan", float("nan"))
        span.set_metric("inf", math.inf)
        span.set_metric("text", "x")  # type: ignore[arg-type]
    (wire,) = sent(trace_agent, tracer)
    assert wire["meta"]["a"] == "1"
    assert wire["meta"]["flag"] == "true"
    assert wire["meta"]["off"] == "false"
    assert "none" not in wire["meta"]
    assert wire["metrics"]["n"] == 3
    assert not {"nan", "inf", "text"} & set(wire["metrics"])


def test_span_attributes_written_directly_still_produce_a_valid_payload(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("op") as span:
        span.metrics["nan"] = float("nan")
        span.meta["n"] = 5  # type: ignore[assignment]
    (wire,) = sent(trace_agent, tracer)
    assert "nan" not in wire["metrics"]
    assert wire["meta"]["n"] == "5"


# -- context -----------------------------------------------------------------------


def test_nesting_sets_parent_and_trace_and_restores_the_active_span(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    assert tracer.current_span() is None
    with tracer.trace("a") as a:
        assert tracer.current_span() is a
        with tracer.trace("b") as b:
            assert tracer.current_span() is b
            with tracer.trace("c") as c:
                pass
        assert tracer.current_span() is a
        with tracer.trace("d") as d:
            pass
    assert tracer.current_span() is None
    spans = by_name(sent(trace_agent, tracer))
    assert spans["b"]["parent_id"] == a.span_id
    assert spans["c"]["parent_id"] == b.span_id
    assert spans["d"]["parent_id"] == a.span_id  # a sibling of b, not a child of it
    assert {s["trace_id"] for s in spans.values()} == {a.trace_id}
    assert c.trace_id == d.trace_id == a.trace_id


def test_current_trace_context_matches_the_active_span(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    assert tracer.current_trace_context() is None
    with tracer.trace("a") as a:
        assert tracer.current_trace_context() == Context(a.trace_id, a.span_id, 1)


def test_finishing_out_of_order_does_not_corrupt_the_stack(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    a = tracer.start_span("a")
    b = tracer.start_span("b")
    a.finish()  # the outer one first
    assert tracer.current_span() is b
    b.finish()
    assert tracer.current_span() is None  # b's saved parent (a) is finished, so skipped


def test_activate_false_leaves_the_active_span_alone(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    with tracer.trace("root") as root:
        leaf = tracer.start_span("leaf", activate=False)
        assert tracer.current_span() is root
        assert leaf.parent_id == root.span_id
        leaf.finish()
        assert tracer.current_span() is root


def test_finishing_from_another_context_is_harmless(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    span = tracer.start_span("a")
    # A ContextVar token would raise here ("created in a different Context").
    done = threading.Thread(target=span.finish)
    done.start()
    done.join()
    assert span.finished
    # This context still *holds* the finished span, but nothing treats it as active.
    assert tracer.current_span() is None
    with tracer.trace("next") as nxt:
        assert nxt.parent_id is None


def test_context_survives_await_and_is_copied_into_tasks(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    async def child() -> str:
        await asyncio.sleep(0)
        span = tracer.current_span()
        assert span is not None
        return span.name

    async def run() -> None:
        async with tracer.trace("root"):
            await asyncio.sleep(0)
            assert tracer.current_span() is not None
            assert await asyncio.create_task(child()) == "root"

            # A span opened inside the task cannot disturb the spawner.
            async def inner() -> None:
                async with tracer.trace("in-task"):
                    await asyncio.sleep(0)

            await asyncio.create_task(inner())
            assert tracer.current_span() is not None
            assert tracer.current_span().name == "root"  # type: ignore[union-attr]

    asyncio.run(run())
    spans = by_name(sent(trace_agent, tracer))
    assert spans["in-task"]["parent_id"] == spans["root"]["span_id"]


def test_one_hundred_interleaved_tasks_never_cross_parent(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    async def worker(i: int) -> None:
        async with tracer.trace("outer", tags={"i": i}) as outer:
            for _ in range(3):
                await asyncio.sleep(0)  # yield so the tasks interleave
            async with tracer.trace("inner", tags={"i": i}) as inner:
                await asyncio.sleep(0)
                assert inner.parent_id == outer.span_id
                assert inner.trace_id == outer.trace_id

    async def run() -> None:
        await asyncio.gather(*(worker(i) for i in range(100)))

    asyncio.run(run())
    spans = sent(trace_agent, tracer)
    assert len(spans) == 200
    outers = {s["meta"]["i"]: s for s in spans if s["name"] == "outer"}
    inners = {s["meta"]["i"]: s for s in spans if s["name"] == "inner"}
    assert len(outers) == len(inners) == 100
    assert len({s["trace_id"] for s in outers.values()}) == 100
    for i in outers:
        assert inners[i]["parent_id"] == outers[i]["span_id"]
        assert inners[i]["trace_id"] == outers[i]["trace_id"]


def test_a_thousand_concurrent_tasks(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    async def one(i: int) -> None:
        async with tracer.trace("t", tags={"i": i}):
            await asyncio.sleep(0)
            with tracer.trace("c"):
                await asyncio.sleep(0)

    async def run() -> None:
        await asyncio.gather(*(one(i) for i in range(1000)))

    asyncio.run(run())
    stats = tracer.stats()
    assert stats.spans_started == stats.spans_finished == 2000
    spans = sent(trace_agent, tracer)
    assert len(spans) == 2000
    assert tracer.stats().chunks_dropped == 0


def test_threads_do_not_inherit_the_active_span(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    seen: dict[str, Any] = {}

    def in_thread() -> None:
        seen["current"] = tracer.current_span()
        with tracer.trace("t") as span:
            seen["trace_id"] = span.trace_id

    with tracer.trace("root") as root:
        thread = threading.Thread(target=in_thread)
        thread.start()
        thread.join()
    assert seen["current"] is None
    assert seen["trace_id"] != root.trace_id  # a bare thread starts its own trace


def test_wrap_executor_carries_the_context_into_the_thread(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with concurrent.futures.ThreadPoolExecutor(2) as pool:
        wrapped = tracer.wrap_executor(pool)
        with tracer.trace("root") as root:

            def work() -> str:
                with tracer.trace("in-thread") as span:
                    assert span.parent_id == root.span_id
                    return span.trace_id

            assert wrapped.submit(work).result() == root.trace_id
            assert list(wrapped.map(lambda x: x * 2, [1, 2])) == [2, 4]  # Executor.map works
        # ...whereas the bare pool does not.
        with tracer.trace("root2") as root2:
            assert pool.submit(lambda: tracer.current_span()).result() is None
            assert root2.span_id
    wrapped.shutdown()


def test_to_thread_and_run_in_executor_with_a_wrapped_executor(
    make_tracer: TracerFactory,
) -> None:
    tracer = make_tracer()

    async def run() -> tuple[str | None, str | None]:
        async with tracer.trace("root"):
            via_to_thread = await asyncio.to_thread(
                lambda: s.name if (s := tracer.current_span()) else None
            )
            with concurrent.futures.ThreadPoolExecutor(1) as pool:
                wrapped = tracer.wrap_executor(pool)
                loop = asyncio.get_running_loop()
                via_executor = await loop.run_in_executor(
                    wrapped, lambda: s.name if (s := tracer.current_span()) else None
                )
            return via_to_thread, via_executor

    assert asyncio.run(run()) == ("root", "root")


def test_child_of_a_context_makes_a_new_local_root(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    upstream = Context("a" * 32, "b" * 16, 2)
    with tracer.trace("entry", child_of=upstream) as span:
        assert span.trace_id == "a" * 32
        assert span.parent_id == "b" * 16
        assert span.context.sampling_priority == 2  # inherited, not re-decided
        assert tracer.current_span() is span
    (wire,) = sent(trace_agent, tracer)
    assert wire["metrics"]["_top_level"] == 1  # the entry span of *this* service
    assert wire["metrics"]["_sampling_priority"] == 2


def test_child_of_a_span_object(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()
    with tracer.trace("a") as a:
        pass
    # a is finished, but a detached child may still name it as parent.
    late = tracer.start_span("late", child_of=a, activate=False)
    late.finish()
    spans = by_name(sent(trace_agent, tracer))
    assert spans["late"]["parent_id"] == a.span_id
    assert spans["late"]["trace_id"] == a.trace_id


def test_top_level_is_set_for_roots_and_service_boundaries_only(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("root"):
        with tracer.trace("same-service"):
            pass
        with tracer.trace("other-service", service="db"), tracer.trace("under-other"):
            pass
    spans = by_name(sent(trace_agent, tracer))
    top = {n: s["metrics"].get("_top_level") for n, s in spans.items()}
    assert top == {"root": 1, "same-service": None, "other-service": 1, "under-other": None}
    assert spans["under-other"]["service"] == "db"  # inherited from its parent


# -- buffer ------------------------------------------------------------------------


def test_nothing_is_sent_until_the_local_root_finishes_then_one_chunk(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("root"):
        with tracer.trace("a"):
            pass
        with tracer.trace("b"):
            pass
        tracer.flush()
        assert trace_agent.chunks() == []  # children finished, root not: still buffered
    tracer.flush()
    chunks = trace_agent.chunks()
    assert len(chunks) == 1
    assert {s["name"] for s in chunks[0]} == {"root", "a", "b"}


def test_two_traces_are_two_chunks(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()
    with tracer.trace("one"):
        pass
    with tracer.trace("two"):
        pass
    tracer.flush()
    assert [len(c) for c in trace_agent.chunks()] == [1, 1]


def test_partial_flush_past_500_finished_spans(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(trace_sample_rate=0.0)
    with tracer.trace("root"):
        for _ in range(500):
            tracer.start_span("child").finish()
        tracer.flush()
        assert trace_agent.chunks() == []  # exactly 500 is not "more than 500"
        tracer.start_span("child").finish()  # the 501st
        tracer.flush()
        (partial,) = trace_agent.chunks()
        assert len(partial) == 501
        # A partial chunk has no root to carry the decision, so its first span does:
        # without it the agent's priority sampler would judge the chunk by nothing.
        assert partial[0]["metrics"]["_sampling_priority"] == 0
    tracer.flush()
    chunks = trace_agent.chunks()
    assert [len(c) for c in chunks] == [501, 1]
    assert chunks[1][0]["name"] == "root"


def test_a_span_finishing_after_its_root_is_sent_as_a_late_chunk(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("root") as root:
        detached = tracer.start_span("detached", activate=False)
    tracer.flush()
    assert [[s["name"] for s in c] for c in trace_agent.chunks()] == [["root"]]
    detached.finish()
    tracer.flush()
    late = trace_agent.chunks()[1]
    assert [s["name"] for s in late] == ["detached"]
    assert late[0]["trace_id"] == root.trace_id
    assert late[0]["parent_id"] == root.span_id
    assert "_sampling_priority" in late[0]["metrics"]


def test_unfinished_spans_are_never_sent(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("root"):
        tracer.start_span("never-finished", activate=False)
    spans = sent(trace_agent, tracer)
    assert [s["name"] for s in spans] == ["root"]


# -- sampling ----------------------------------------------------------------------


def test_rate_zero_traces_are_still_sent_with_priority_zero(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(trace_sample_rate=0.0)
    with tracer.trace("root"), tracer.trace("child"):
        pass
    spans = by_name(sent(trace_agent, tracer))
    assert set(spans) == {"root", "child"}  # the agent needs 100% for its statistics
    assert spans["root"]["metrics"]["_sampling_priority"] == 0
    assert "_sampling_priority" not in spans["child"]["metrics"]  # the root carries it


def test_rate_one_keeps_everything(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer(trace_sample_rate=1.0)
    for _ in range(20):
        with tracer.trace("t"):
            pass
    assert {s["metrics"]["_sampling_priority"] for s in sent(trace_agent, tracer)} == {1}


def test_the_decision_follows_the_shared_function_of_the_trace_id(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    from ozy._tracing import sample_keep

    tracer = make_tracer(trace_sample_rate=0.3)
    for _ in range(300):
        with tracer.trace("t"):
            pass
    spans = sent(trace_agent, tracer)
    kept = 0
    for s in spans:
        want = 1 if sample_keep(s["trace_id"], 0.3) else 0
        assert s["metrics"]["_sampling_priority"] == want
        kept += want
    assert 40 < kept < 150  # and it is a real sample, not all or nothing


def test_head_decision_is_made_once_children_inherit_it(make_tracer: TracerFactory) -> None:
    tracer = make_tracer(trace_sample_rate=0.0)
    with tracer.trace("root") as root:
        with tracer.trace("child") as child:
            assert child.context.sampling_priority == 0
        assert root.context.sampling_priority == 0
        tracer._config = dataclasses.replace(tracer._config, trace_sample_rate=1.0)
        with tracer.trace("later-child") as later:
            assert later.context.sampling_priority == 0  # not re-decided mid-trace


def test_extracted_priority_is_inherited_whatever_the_local_rate(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(trace_sample_rate=0.0)
    ctx = tracer.extract(
        {"x-ozy-trace-id": "c" * 32, "x-ozy-parent-id": "d" * 16, "x-ozy-sampling-priority": "1"}
    )
    with tracer.trace("entry", child_of=ctx):
        pass
    (span,) = sent(trace_agent, tracer)
    assert span["metrics"]["_sampling_priority"] == 1


def test_rate_by_service_from_the_agent_overrides_the_local_rate(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(trace_sample_rate=1.0)
    trace_agent.response = {"rate_by_service": {"service:svc,env:test": 0.0}}
    with tracer.trace("first"):
        pass
    tracer.flush()  # the response carries the new rates
    with tracer.trace("second"):
        pass
    spans = by_name(sent(trace_agent, tracer))
    assert spans["first"]["metrics"]["_sampling_priority"] == 1  # decided before the rates came
    assert spans["second"]["metrics"]["_sampling_priority"] == 0


def test_rate_by_service_applies_only_to_the_matching_service_and_env(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(trace_sample_rate=1.0)
    trace_agent.response = {
        "rate_by_service": {"service:other,env:test": 0.0, "service:svc,env:prod": 0.0}
    }
    with tracer.trace("first"):
        pass
    tracer.flush()
    with tracer.trace("second"):
        pass
    with tracer.trace("explicit-other", service="other"):
        pass
    spans = by_name(sent(trace_agent, tracer))
    assert spans["second"]["metrics"]["_sampling_priority"] == 1
    assert spans["explicit-other"]["metrics"]["_sampling_priority"] == 0


def test_a_later_response_replaces_the_earlier_rates(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    trace_agent.response = {"rate_by_service": {"service:svc,env:test": 0.0}}
    with tracer.trace("a"):
        pass
    tracer.flush()
    trace_agent.response = {"rate_by_service": {}}
    with tracer.trace("b"):
        pass
    tracer.flush()
    with tracer.trace("c"):
        pass
    spans = by_name(sent(trace_agent, tracer))
    assert spans["c"]["metrics"]["_sampling_priority"] == 1


@pytest.mark.parametrize(
    "garbage",
    [
        {"rate_by_service": "nope"},
        {"rate_by_service": {"service:svc,env:test": "0"}},
        {"rate_by_service": {"service:svc,env:test": True}},
        {"rate_by_service": {"service:svc,env:test": None}},
        {"rate_by_service": {"service:svc,env:test": float("nan")}},
        {"rate_by_service": [1, 2]},
        {},
    ],
)
def test_garbage_rates_are_ignored(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent, garbage: dict[str, Any]
) -> None:
    tracer = make_tracer()
    trace_agent.response = garbage
    with tracer.trace("a"):
        pass
    tracer.flush()
    with tracer.trace("b"):
        pass
    spans = by_name(sent(trace_agent, tracer))
    assert spans["b"]["metrics"]["_sampling_priority"] == 1


def test_rates_are_clamped(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()
    trace_agent.response = {"rate_by_service": {"service:svc,env:test": -5}}
    with tracer.trace("a"):
        pass
    tracer.flush()
    assert tracer._rates == {"service:svc,env:test": 0.0}
    tracer._set_rates({"service:svc,env:test": 99})
    assert tracer._rates == {"service:svc,env:test": 1.0}


# -- wire limits -------------------------------------------------------------------


def test_the_agents_normalization_limits_are_applied_in_the_sdk(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("n" * 300, resource="r" * 6000, service="s" * 300, type="bogus") as span:
        span.set_tag("long", "x" * 6000)
        span.set_tag("k" * 101, "dropped: key too long")
        for i in range(150):
            span.set_tag(f"t{i:03d}", "v")
        for i in range(80):
            span.set_metric(f"m{i:03d}", i)
    (wire,) = sent(trace_agent, tracer)
    assert len(wire["name"]) == 100
    assert len(wire["service"]) == 100
    assert len(wire["resource"]) == 5000
    assert wire["type"] == "custom"
    assert len(wire["meta"]) == 100
    assert "k" * 101 not in wire["meta"]
    assert len(wire["metrics"]) == 50
    # Dropped in sorted key order, so the same span always normalizes the same way, and the
    # reserved keys the agent relies on survive the cut.
    assert wire["metrics"]["_top_level"] == 1
    assert wire["metrics"]["_sampling_priority"] == 1
    assert "m000" in wire["metrics"]
    assert "m079" not in wire["metrics"]
    assert all(len(v) <= 5000 for v in wire["meta"].values())


def test_truncation_never_splits_a_character(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    # 2-byte characters: 5000 bytes is 2500 of them; an odd cut would be invalid UTF-8.
    with tracer.trace("op", resource="é" * 3000) as span:
        span.set_tag("emoji", "😀" * 2000)
    (wire,) = sent(trace_agent, tracer)
    assert wire["resource"] == "é" * 2500
    assert len(wire["meta"]["emoji"].encode()) <= 5000
    assert wire["meta"]["emoji"] == "😀" * 1250


def test_empty_names_and_services_become_valid(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer(service=None)
    with tracer.trace(""):
        pass
    (wire,) = sent(trace_agent, tracer)
    assert wire["name"] == "unnamed"
    assert wire["service"] == "unknown"
    assert "env" in wire["meta"]


def test_every_span_validates_against_the_wire_rules(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("root", resource="r") as root:
        with tracer.trace("c1"):
            pass
        root.set_error(RuntimeError("x"))
    spans = sent(trace_agent, tracer)
    for s in spans:
        assert HEX32.fullmatch(s["trace_id"])
        assert s["trace_id"] != "0" * 32
        assert HEX16.fullmatch(s["span_id"])
        assert s["span_id"] != "0" * 16
        assert s["parent_id"] is None or HEX16.fullmatch(s["parent_id"])
        assert s["parent_id"] != s["span_id"]
        assert s["error"] in (0, 1)
        assert s["duration"] >= 0
        assert s["start"] > 10**15  # microseconds, not seconds or milliseconds
        assert all(isinstance(v, float | int) and math.isfinite(v) for v in s["metrics"].values())


def test_body_envelope_and_path(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()
    with tracer.trace("op"):
        pass
    tracer.flush()
    assert trace_agent.paths == ["/v1/traces"]
    body = json.loads(trace_agent.raw[0])
    assert body["tracer"]["lang"] == "python"
    assert body["tracer"]["version"] == ozy.__version__
    assert re.fullmatch(r"3\.\d+\.\d+.*", body["tracer"]["lang_version"])
    assert isinstance(body["traces"], list)


# -- disabled / safety -------------------------------------------------------------


def test_disabled_tracer_runs_your_code_and_records_nothing() -> None:
    tracer = Tracer()
    assert not tracer.enabled
    ran = []
    with tracer.trace("op", tags={"a": 1}) as span:
        span.set_tag("x", "y")
        span.set_metric("m", 1)
        span.set_error(ValueError("x"))
        span.finish()
        ran.append(tracer.current_span())
    assert ran == [None]  # a no-op span is never activated
    assert tracer.current_trace_context() is None
    assert tracer.stats() == ozy.TracerStats()
    assert tracer.inject({}) == {}
    carrier: dict[str, str] = {}
    tracer.inject(carrier, Context("a" * 32, "b" * 16, 1))
    assert carrier == {}
    tracer.flush()
    tracer.close()


def test_disabled_tracer_still_propagates_exceptions_unchanged() -> None:
    tracer = Tracer()
    with pytest.raises(KeyError), tracer.trace("op"):
        raise KeyError("k")


def test_disabled_means_no_thread_no_hooks(monkeypatch: pytest.MonkeyPatch) -> None:
    import atexit
    import os

    registered: list[Any] = []
    monkeypatch.setattr(atexit, "register", lambda fn, *a: registered.append(fn))
    monkeypatch.setattr(os, "register_at_fork", lambda **kw: registered.append(kw))
    before = {t.name for t in threading.enumerate()}
    tracer = Tracer()
    tracer.configure(Config())  # no agent host
    tracer.configure(Config(agent_host="h", trace_enabled=False))
    with tracer.trace("op"):
        pass
    assert registered == []
    assert {t.name for t in threading.enumerate()} == before


def test_trace_enabled_false_disables_even_with_an_agent_host(
    trace_agent: FakeTraceAgent,
) -> None:
    tracer = Tracer()
    tracer.configure(
        Config(agent_host="127.0.0.1", trace_port=trace_agent.port, trace_enabled=False)
    )
    with tracer.trace("op"):
        pass
    tracer.flush()
    assert not tracer.enabled
    assert trace_agent.bodies == []


def test_a_failing_id_source_degrades_to_a_noop_span(make_tracer: TracerFactory) -> None:
    def boom(n: int) -> bytes:
        raise OSError("no entropy")

    tracer = make_tracer(random_bytes=boom)
    ran = []
    with tracer.trace("op") as span:
        ran.append(span.span_id)
    assert ran == ["0" * 16]  # the no-op span: our failure never reached the host


def test_a_failing_tag_value_never_raises(make_tracer: TracerFactory) -> None:
    class Bad:
        def __str__(self) -> str:
            raise RuntimeError("no")

    tracer = make_tracer()
    with tracer.trace("op", tags={"k": Bad()}) as span:
        span.set_tag("also", Bad())
    assert span.finished


def test_stats_count_spans_and_chunks(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()
    with tracer.trace("a"):
        tracer.start_span("b").finish()
    assert tracer.stats().spans_started == 2
    assert tracer.stats().spans_finished == 2
    tracer.flush()
    stats = tracer.stats()
    assert stats.chunks_sent == 1
    assert stats.chunks_dropped == 0
    assert stats.send_errors == 0


# -- wrap --------------------------------------------------------------------------


def test_wrap_sync_async_and_bare(make_tracer: TracerFactory, trace_agent: FakeTraceAgent) -> None:
    tracer = make_tracer()

    @tracer.wrap("cover.resize", type="worker", tags={"k": "v"})
    def resize(x: int, *, y: int = 1) -> int:
        """Doc."""
        return x + y

    @tracer.wrap
    async def fetch(x: int) -> int:
        return x * 2

    @tracer.wrap(resource="custom-resource")
    def named_by_function() -> None:
        pass

    assert resize.__name__ == "resize"
    assert resize.__doc__ == "Doc."
    assert resize(1, y=2) == 3
    assert asyncio.run(fetch(4)) == 8
    named_by_function()
    spans = by_name(sent(trace_agent, tracer))
    assert spans["cover.resize"]["type"] == "worker"
    assert spans["cover.resize"]["meta"]["k"] == "v"
    fetch_name = next(n for n in spans if n.endswith("fetch"))
    assert spans[fetch_name]["name"].endswith("test_wrap_sync_async_and_bare.<locals>.fetch")
    other = next(s for n, s in spans.items() if n.endswith("named_by_function"))
    assert other["resource"] == "custom-resource"


def test_wrap_reraises_and_marks_errors_for_sync_and_async(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    @tracer.wrap("sync.fail")
    def sync_fail() -> None:
        raise RuntimeError("sync")

    @tracer.wrap("async.fail")
    async def async_fail() -> None:
        raise RuntimeError("async")

    with pytest.raises(RuntimeError, match="sync"):
        sync_fail()
    with pytest.raises(RuntimeError, match="async"):
        asyncio.run(async_fail())
    spans = by_name(sent(trace_agent, tracer))
    assert spans["sync.fail"]["error"] == spans["async.fail"]["error"] == 1


def test_wrapped_calls_nest_under_the_active_span(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer = make_tracer()

    @tracer.wrap("inner")
    def inner() -> None:
        pass

    with tracer.trace("outer") as outer:
        inner()
    spans = by_name(sent(trace_agent, tracer))
    assert spans["inner"]["parent_id"] == outer.span_id


def test_wrap_on_a_disabled_tracer_is_transparent() -> None:
    tracer = Tracer()

    @tracer.wrap("x")
    def f(a: int) -> int:
        return a + 1

    assert f(1) == 2


# -- propagation -------------------------------------------------------------------


def test_inject_writes_the_three_headers_of_the_active_span(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    with tracer.trace("a") as span:
        carrier = tracer.inject({})
    assert carrier == {
        "x-ozy-trace-id": span.trace_id,
        "x-ozy-parent-id": span.span_id,
        "x-ozy-sampling-priority": "1",
    }


def test_inject_without_a_context_writes_nothing(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    assert tracer.inject({"keep": "me"}) == {"keep": "me"}


def test_inject_into_an_unwritable_carrier_does_not_raise(make_tracer: TracerFactory) -> None:
    from types import MappingProxyType

    tracer = make_tracer()
    with tracer.trace("a"):
        tracer.inject(MappingProxyType({}))  # type: ignore[arg-type]


def test_extract_inject_round_trip(make_tracer: TracerFactory) -> None:
    import random

    tracer = make_tracer()
    rng = random.Random(3)
    for _ in range(200):
        ctx = Context(
            rng.getrandbits(128).to_bytes(16, "big").hex(),
            rng.getrandbits(64).to_bytes(8, "big").hex(),
            rng.choice([-1, 0, 1, 2]),
        )
        if set(ctx.trace_id) == {"0"} or set(ctx.span_id) == {"0"}:
            continue
        assert tracer.extract(tracer.inject({}, ctx)) == ctx


def test_extract_reads_headers_in_any_case_and_bytes(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    want = Context("a" * 32, "b" * 16, 0)
    assert (
        tracer.extract(
            {
                "X-Ozy-Trace-Id": "A" * 32,
                "X-OZY-PARENT-ID": "B" * 16,
                "x-ozy-sampling-priority": "0",
            }
        )
        == want
    )
    assert (
        tracer.extract(
            {
                b"x-ozy-trace-id": b"a" * 32,
                b"x-ozy-parent-id": b"b" * 16,
                b"x-ozy-sampling-priority": b"0",
            }
        )
        == want
    )


def test_extract_reads_the_job_carrier_form(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    assert tracer.extract(
        {"trace_id": "a" * 32, "parent_id": "b" * 16, "sampling_priority": 2}
    ) == Context("a" * 32, "b" * 16, 2)


def test_extract_of_a_half_present_carrier_is_none(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    assert tracer.extract({"x-ozy-trace-id": "a" * 32}) is None
    assert tracer.extract({"x-ozy-parent-id": "b" * 16}) is None
    assert (
        tracer.extract({"x-ozy-trace-id": "zz", "trace_id": "a" * 32, "parent_id": "b" * 16})
        is None
    )


# -- fork --------------------------------------------------------------------------


def test_after_fork_the_child_gets_fresh_state(make_tracer: TracerFactory) -> None:
    tracer = make_tracer()
    old_lock = tracer._lock
    writer = tracer._writer
    assert writer is not None
    with tracer.trace("a"):
        pass
    assert writer.queued == 1
    tracer._after_fork_in_child()
    assert tracer._lock is not old_lock
    assert writer.queued == 0  # the parent still owns and sends those chunks
    assert writer._thread is None
    with tracer.trace("b"):  # and the child's first finished trace restarts a flusher
        pass
    assert writer._thread is not None
