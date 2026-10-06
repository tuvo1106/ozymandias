"""arq: the trace crosses the queue (inject -> traced job) in one trace, carrier stripped."""

from __future__ import annotations

import asyncio
import datetime as dt
import pickle
from types import SimpleNamespace
from typing import Any

import arq
import pytest
from arq.connections import ArqRedis
from arq.cron import CronJob
from arq.worker import Function, Retry

import ozy
from ozy import Context
from ozy.integrations.arq import (
    CARRIER_KWARG,
    INTEGRATION,
    inject_job_kwargs,
    traced_job,
)
from ozy.integrations.arq import (
    traced as trace_job,
)

from .conftest import FakeTraceAgent


def by_name(agent: FakeTraceAgent) -> dict[str, dict[str, Any]]:
    ozy.tracer.flush()
    return {s["name"]: s for s in agent.spans()}


def ctx_for(**extra: Any) -> dict[str, Any]:
    return {
        "job_id": "job-1",
        "job_try": 1,
        "enqueue_time": dt.datetime.now(dt.UTC) - dt.timedelta(milliseconds=250),
        **extra,
    }


# -- producer ----------------------------------------------------------------------


def test_inject_adds_the_carrier_inside_an_enqueue_span(traced: FakeTraceAgent) -> None:
    kwargs = {"submission_id": 7}
    with ozy.tracer.trace("request") as root:
        out = inject_job_kwargs(kwargs, function="judge")
    assert kwargs == {"submission_id": 7}  # the caller's dict is not mutated
    assert out["submission_id"] == 7
    carrier = out[CARRIER_KWARG]
    assert set(carrier) == {"trace_id", "parent_id", "sampling_priority"}
    spans = by_name(traced)
    enqueue = spans["arq.enqueue"]
    assert enqueue["type"] == "queue"
    assert enqueue["resource"] == "judge"
    assert enqueue["meta"]["span.kind"] == "producer"
    assert enqueue["parent_id"] == root.span_id
    # The enqueue span is the parent the job will attach to.
    assert carrier == {
        "trace_id": root.trace_id,
        "parent_id": enqueue["span_id"],
        "sampling_priority": 1,
    }


def test_inject_outside_a_trace_starts_a_root_enqueue_trace(traced: FakeTraceAgent) -> None:
    out = inject_job_kwargs({})
    spans = by_name(traced)
    assert spans["arq.enqueue"]["parent_id"] is None
    assert out[CARRIER_KWARG]["trace_id"] == spans["arq.enqueue"]["trace_id"]


def test_inject_with_tracing_disabled_returns_plain_kwargs() -> None:
    assert inject_job_kwargs({"a": 1}) == {"a": 1}  # no carrier a job would have to strip


def test_the_carrier_survives_a_pickle_round_trip_and_extract(traced: FakeTraceAgent) -> None:
    with ozy.tracer.trace("request"):
        out = inject_job_kwargs({})
    wire = pickle.loads(pickle.dumps(out))  # what arq does to job kwargs
    ctx = ozy.tracer.extract(wire[CARRIER_KWARG])
    assert ctx == Context(out[CARRIER_KWARG]["trace_id"], out[CARRIER_KWARG]["parent_id"], 1)


# -- consumer ----------------------------------------------------------------------


def test_the_job_continues_the_trace_and_never_sees_the_carrier(traced: FakeTraceAgent) -> None:
    seen: dict[str, Any] = {}

    @traced_job
    async def judge(ctx: dict[str, Any], submission_id: int, *, flag: bool = False) -> str:
        seen["args"] = (submission_id, flag)
        seen["kwargs_has_carrier"] = CARRIER_KWARG in locals()
        seen["span"] = ozy.tracer.current_span()
        return "verdict"

    with ozy.tracer.trace("request") as root:
        kwargs = inject_job_kwargs({"submission_id": 7, "flag": True}, function="judge")
    assert asyncio.run(judge(ctx_for(), **kwargs)) == "verdict"
    assert seen["args"] == (7, True)

    spans = by_name(traced)
    enqueue, job = spans["arq.enqueue"], spans["arq.job"]
    assert job["trace_id"] == root.trace_id
    assert job["parent_id"] == enqueue["span_id"]  # the job hangs under the enqueue span
    assert job["type"] == "worker"
    assert job["resource"].endswith("judge")
    assert job["meta"]["span.kind"] == "consumer"
    assert job["meta"]["job.id"] == "job-1"
    assert job["meta"]["job.try"] == "1"
    assert 200 <= job["metrics"]["queue.wait_ms"] < 5000
    assert job["metrics"]["_top_level"] == 1  # the entry span of the worker service
    assert job["error"] == 0


def test_the_wrapped_function_receives_exactly_the_kwargs_it_was_enqueued_with(
    traced: FakeTraceAgent,
) -> None:
    received: list[dict[str, Any]] = []

    @trace_job
    async def strict(ctx: dict[str, Any], a: int, b: int = 0) -> None:
        received.append({"a": a, "b": b})

    with ozy.tracer.trace("request"):
        kwargs = inject_job_kwargs({"a": 1, "b": 2})
    asyncio.run(strict(ctx_for(), **kwargs))  # a stray _ozymandias kwarg would be a TypeError
    assert received == [{"a": 1, "b": 2}]


def test_the_carrier_is_stripped_even_when_tracing_is_disabled() -> None:
    received: list[dict[str, Any]] = []

    @trace_job
    async def job(ctx: dict[str, Any], **kwargs: Any) -> None:
        received.append(kwargs)

    carrier = {"trace_id": "a" * 32, "parent_id": "b" * 16, "sampling_priority": 1}
    asyncio.run(job({}, x=1, **{CARRIER_KWARG: carrier}))
    assert received == [{"x": 1}]  # a worker with tracing off must not choke on it either


def test_a_legacy_job_without_a_carrier_still_runs_and_starts_a_root_trace(
    traced: FakeTraceAgent,
) -> None:
    @trace_job
    async def legacy(ctx: dict[str, Any], x: int) -> int:
        return x + 1

    assert asyncio.run(legacy(ctx_for(), x=1)) == 2
    job = by_name(traced)["arq.job"]
    assert job["parent_id"] is None
    assert job["metrics"]["_top_level"] == 1


@pytest.mark.parametrize(
    "carrier",
    [
        "not a dict",
        {},
        {"trace_id": "short", "parent_id": "b" * 16, "sampling_priority": 1},
        {"trace_id": "a" * 32, "parent_id": "0" * 16, "sampling_priority": 1},
        {"trace_id": "a" * 32, "parent_id": "b" * 16, "sampling_priority": 99},
        None,
        42,
    ],
)
def test_a_malformed_carrier_starts_a_fresh_trace_and_the_job_runs(
    traced: FakeTraceAgent, carrier: Any
) -> None:
    ran: list[bool] = []

    @trace_job
    async def job(ctx: dict[str, Any]) -> None:
        ran.append(True)

    asyncio.run(job(ctx_for(), **{CARRIER_KWARG: carrier}))
    assert ran == [True]
    assert by_name(traced)["arq.job"]["parent_id"] is None


def test_an_exception_marks_the_span_and_reraises_the_same_object(traced: FakeTraceAgent) -> None:
    boom = RuntimeError("judge crashed")

    @trace_job
    async def failing(ctx: dict[str, Any]) -> None:
        raise boom

    with pytest.raises(RuntimeError) as caught:
        asyncio.run(failing(ctx_for()))
    assert caught.value is boom
    job = by_name(traced)["arq.job"]
    assert job["error"] == 1
    assert job["meta"]["error.type"] == "RuntimeError"
    assert job["meta"]["error.message"] == "judge crashed"


def test_arq_retry_is_not_an_error(traced: FakeTraceAgent) -> None:
    @trace_job
    async def flaky(ctx: dict[str, Any]) -> None:
        raise Retry(defer=1)

    with pytest.raises(Retry):
        asyncio.run(flaky(ctx_for()))
    job = by_name(traced)["arq.job"]
    assert job["error"] == 0
    assert job["meta"]["job.retry"] == "true"


def test_cancellation_ends_the_span_without_calling_it_an_error(traced: FakeTraceAgent) -> None:
    @trace_job
    async def slow(ctx: dict[str, Any]) -> None:
        await asyncio.sleep(10)

    async def run() -> None:
        task = asyncio.create_task(slow(ctx_for()))
        await asyncio.sleep(0.01)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task

    asyncio.run(run())
    assert by_name(traced)["arq.job"]["error"] == 0


def test_spans_made_inside_the_job_are_its_children(traced: FakeTraceAgent) -> None:
    @trace_job
    async def job(ctx: dict[str, Any]) -> None:
        with ozy.tracer.trace("inside"):
            pass

    asyncio.run(job(ctx_for()))
    spans = by_name(traced)
    assert spans["inside"]["parent_id"] == spans["arq.job"]["span_id"]


def test_name_and_signature_are_preserved() -> None:
    @trace_job
    async def my_job(ctx: dict[str, Any], x: int) -> None:
        """Docstring."""

    assert my_job.__name__ == "my_job"
    assert my_job.__qualname__.endswith("my_job")
    assert my_job.__doc__ == "Docstring."
    assert asyncio.iscoroutinefunction(my_job)


def test_arq_func_options_are_preserved() -> None:
    async def coro(ctx: dict[str, Any]) -> None:
        pass

    original = arq.func(coro, name="custom-name", timeout=12, max_tries=3, keep_result=5)
    wrapped = trace_job(original)
    assert isinstance(wrapped, Function)
    assert (wrapped.name, wrapped.timeout_s, wrapped.max_tries, wrapped.keep_result_s) == (
        "custom-name",
        12,
        3,
        5,
    )
    assert wrapped.coroutine is not original.coroutine
    assert original.coroutine is coro  # the caller's object is untouched


def test_a_cron_job_keeps_its_schedule_and_starts_a_new_root_trace_per_run(
    traced: FakeTraceAgent,
) -> None:
    async def nightly(ctx: dict[str, Any]) -> None:
        pass

    cron = arq.cron(nightly, hour=3, minute=30, run_at_startup=True, unique=True)
    wrapped = trace_job(cron)
    assert isinstance(wrapped, CronJob)
    assert (wrapped.hour, wrapped.minute, wrapped.run_at_startup, wrapped.unique) == (
        cron.hour,
        cron.minute,
        True,
        True,
    )
    assert wrapped.name == cron.name

    async def run_twice() -> None:
        await wrapped.coroutine({})
        await wrapped.coroutine({})

    asyncio.run(run_twice())
    ozy.tracer.flush()
    jobs = [s for s in traced.spans() if s["name"] == "arq.job"]
    assert len(jobs) == 2
    assert all(j["parent_id"] is None for j in jobs)
    assert jobs[0]["trace_id"] != jobs[1]["trace_id"]  # each run is its own trace


def test_wrapping_twice_does_not_nest_job_spans(traced: FakeTraceAgent) -> None:
    @trace_job
    async def job(ctx: dict[str, Any]) -> None:
        pass

    again = trace_job(job)
    assert again is job
    asyncio.run(again(ctx_for()))
    ozy.tracer.flush()
    assert len([s for s in traced.spans() if s["name"] == "arq.job"]) == 1

    async def coro(ctx: dict[str, Any]) -> None:
        pass

    function = trace_job(arq.func(coro))
    assert trace_job(function) is function


def test_a_context_without_arq_fields_is_tolerated(traced: FakeTraceAgent) -> None:
    @trace_job
    async def job(ctx: Any) -> None:
        pass

    asyncio.run(job({}))
    asyncio.run(job(None))
    asyncio.run(job({"enqueue_time": "yesterday", "job_try": None}))
    ozy.tracer.flush()
    assert len([s for s in traced.spans() if s["name"] == "arq.job"]) == 3


def test_queue_wait_is_never_negative(traced: FakeTraceAgent) -> None:
    @trace_job
    async def job(ctx: dict[str, Any]) -> None:
        pass

    future = dt.datetime.now(dt.UTC) + dt.timedelta(seconds=30)  # clock skew between hosts
    asyncio.run(job({"enqueue_time": future}))
    assert by_name(traced)["arq.job"]["metrics"]["queue.wait_ms"] == 0


def test_naive_enqueue_times_are_read_as_local_time(traced: FakeTraceAgent) -> None:
    @trace_job
    async def job(ctx: dict[str, Any]) -> None:
        pass

    asyncio.run(job({"enqueue_time": dt.datetime.now() - dt.timedelta(seconds=2)}))
    wait = by_name(traced)["arq.job"]["metrics"]["queue.wait_ms"]
    assert 1500 < wait < 6000


# -- the patch: ArqRedis.enqueue_job ------------------------------------------------


@pytest.fixture
def fake_enqueue(monkeypatch: pytest.MonkeyPatch) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []

    async def enqueue_job(self: Any, function: str, *args: Any, **kwargs: Any) -> Any:
        calls.append({"function": function, "args": args, "kwargs": kwargs})
        if kwargs.get("_fail"):
            raise ConnectionError("redis down")
        return SimpleNamespace(job_id="enqueued-id") if not kwargs.get("_dup") else None

    monkeypatch.setattr(ArqRedis, "enqueue_job", enqueue_job)
    return calls


def test_patched_enqueue_injects_the_carrier_and_times_the_real_call(
    traced: FakeTraceAgent, fake_enqueue: list[dict[str, Any]]
) -> None:
    INTEGRATION.patch()
    pool = ArqRedis.__new__(ArqRedis)

    async def run() -> Any:
        async with ozy.tracer.trace("request") as root:
            await pool.enqueue_job("judge", 1, _job_id="j", submission_id=7)
            return root

    root = asyncio.run(run())
    (call,) = fake_enqueue
    assert call["args"] == (1,)
    assert call["kwargs"]["submission_id"] == 7
    assert call["kwargs"]["_job_id"] == "j"  # arq's own options pass through untouched
    carrier = call["kwargs"][CARRIER_KWARG]
    enqueue = by_name(traced)["arq.enqueue"]
    assert enqueue["resource"] == "judge"
    assert enqueue["meta"]["job.id"] == "enqueued-id"
    assert enqueue["parent_id"] == root.span_id
    assert carrier["parent_id"] == enqueue["span_id"]
    assert carrier["trace_id"] == root.trace_id


def test_patched_enqueue_outside_a_trace_adds_nothing(
    traced: FakeTraceAgent, fake_enqueue: list[dict[str, Any]]
) -> None:
    INTEGRATION.patch()
    pool = ArqRedis.__new__(ArqRedis)
    asyncio.run(pool.enqueue_job("judge", x=1))
    assert fake_enqueue[0]["kwargs"] == {"x": 1}  # no carrier: nothing to continue
    assert by_name(traced) == {}


def test_patched_enqueue_failure_marks_the_span_and_reraises(
    traced: FakeTraceAgent, fake_enqueue: list[dict[str, Any]]
) -> None:
    INTEGRATION.patch()
    pool = ArqRedis.__new__(ArqRedis)

    async def run() -> None:
        async with ozy.tracer.trace("request"):
            with pytest.raises(ConnectionError, match="redis down"):
                await pool.enqueue_job("judge", _fail=True)

    asyncio.run(run())
    enqueue = by_name(traced)["arq.enqueue"]
    assert enqueue["error"] == 1
    assert enqueue["meta"]["error.type"] == "ConnectionError"


def test_a_duplicate_job_id_returns_none_and_still_finishes_the_span(
    traced: FakeTraceAgent, fake_enqueue: list[dict[str, Any]]
) -> None:
    INTEGRATION.patch()
    pool = ArqRedis.__new__(ArqRedis)

    async def run() -> Any:
        async with ozy.tracer.trace("request"):
            return await pool.enqueue_job("judge", _dup=True)

    assert asyncio.run(run()) is None  # arq's "already exists" answer reaches the caller
    enqueue = by_name(traced)["arq.enqueue"]
    assert "job.id" not in enqueue["meta"]


def test_patch_is_idempotent_and_unpatch_restores(
    traced: FakeTraceAgent, fake_enqueue: list[dict[str, Any]]
) -> None:
    original = ArqRedis.enqueue_job
    INTEGRATION.patch()
    INTEGRATION.patch()
    assert ArqRedis.enqueue_job.__wrapped__ is original  # type: ignore[attr-defined]
    INTEGRATION.unpatch()
    assert ArqRedis.enqueue_job is original


def test_patched_enqueue_with_a_broken_tracer_still_enqueues(
    traced: FakeTraceAgent,
    fake_enqueue: list[dict[str, Any]],
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    INTEGRATION.patch()
    pool = ArqRedis.__new__(ArqRedis)

    class Broken:
        span_id = "1" * 16

        @property
        def context(self) -> Any:
            raise RuntimeError("no context for you")

        def finish(self) -> None:
            pass

        def set_tag(self, *a: Any) -> None:
            pass

    async def run() -> None:
        async with ozy.tracer.trace("request"):
            # From here on every span the integration starts is broken.
            monkeypatch.setattr(ozy.tracer, "start_span", lambda *a, **k: Broken())
            await pool.enqueue_job("judge", x=1)

    asyncio.run(run())
    assert fake_enqueue[0]["kwargs"] == {"x": 1}  # the job was still enqueued, carrier-less


def test_unavailable_library_is_a_noop(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real = builtins.__import__

    def fake(name: str, *a: Any, **k: Any) -> Any:
        if name.startswith("arq"):
            raise ImportError(name)
        return real(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", fake)
    assert INTEGRATION.is_available() is False
    INTEGRATION.patch()
