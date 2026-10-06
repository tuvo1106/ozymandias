"""The trace writer: bounded queue, drop-oldest, never blocking the caller, fork safety."""

from __future__ import annotations

import os
import socket
import time
import warnings
from typing import Any

import pytest

from ozy import Tracer
from ozy._trace_writer import MAX_BODY_BYTES, TraceWriter

from .conftest import FakeTraceAgent, TracerFactory

INFO = {"lang": "python", "lang_version": "3.12.0", "version": "0"}


def span(n: int, **extra: Any) -> dict[str, Any]:
    return {
        "trace_id": f"{n:032x}",
        "span_id": f"{n + 1:016x}",
        "parent_id": None,
        "service": "s",
        "name": f"n{n}",
        "resource": "r",
        "type": "custom",
        "start": 1_790_000_000_000_000,
        "duration": 1,
        "error": 0,
        "meta": {},
        "metrics": {},
        **extra,
    }


def writer_for(agent: FakeTraceAgent, **kwargs: Any) -> TraceWriter:
    settings: dict[str, Any] = {"flush_interval": 3600.0, "flush_chunks": 10**9, "timeout": 2.0}
    settings.update(kwargs)
    return TraceWriter("127.0.0.1", agent.port, tracer_info=INFO, **settings)


def closed_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def test_the_queue_is_bounded_and_drops_the_oldest(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent, max_queue=5)
    for i in range(8):
        writer.submit([span(i)])
    assert writer.queued == 5
    assert writer.chunks_dropped == 3
    writer.flush()
    names = [c[0]["name"] for c in trace_agent.chunks()]
    assert names == ["n3", "n4", "n5", "n6", "n7"]  # recent traces are worth more
    assert writer.chunks_sent == 5
    writer.close()


def test_default_queue_bound_is_1000_chunks(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    for i in range(1200):
        writer.submit([span(i)])
    assert writer.queued == 1000
    assert writer.chunks_dropped == 200
    writer.close()


def test_a_slow_agent_never_blocks_the_caller(trace_agent: FakeTraceAgent) -> None:
    trace_agent.delay = 1.5
    writer = writer_for(trace_agent, flush_interval=0.01, timeout=0.2)
    started = time.perf_counter()
    for i in range(500):
        writer.submit([span(i)])
    elapsed = time.perf_counter() - started
    assert elapsed < 0.5  # 500 submits while the flusher is stuck in a 1.5 s request
    writer.close(0.1)


def test_a_dead_agent_costs_a_counted_error_not_a_retry() -> None:
    writer = TraceWriter(
        "127.0.0.1",
        closed_port(),
        tracer_info=INFO,
        flush_interval=3600.0,
        flush_chunks=10**9,
        timeout=0.5,
    )
    writer.submit([span(1)])
    writer.submit([span(2)])
    writer.flush()
    assert writer.send_errors == 1  # one request for the whole batch, no retries
    assert writer.chunks_dropped == 2
    assert writer.chunks_sent == 0
    assert writer.queued == 0
    writer.close()


@pytest.mark.parametrize("status", [429, 500, 503])
def test_refusals_drop_the_batch_without_retrying(trace_agent: FakeTraceAgent, status: int) -> None:
    trace_agent.status = status
    writer = writer_for(trace_agent)
    writer.submit([span(1)])
    writer.flush()
    writer.flush()  # nothing left to send: the dropped chunk is not retried
    assert len(trace_agent.bodies) == 1
    assert writer.chunks_dropped == 1
    assert writer.send_errors == 1
    assert writer.chunks_sent == 0
    writer.close()


def test_a_request_times_out_instead_of_hanging(trace_agent: FakeTraceAgent) -> None:
    trace_agent.delay = 2.0
    writer = writer_for(trace_agent, timeout=0.2)
    writer.submit([span(1)])
    started = time.perf_counter()
    writer.flush()
    assert time.perf_counter() - started < 1.0
    assert writer.send_errors == 1
    assert writer.chunks_dropped == 1
    writer.close(0.1)


def test_rate_by_service_is_read_from_a_200_response(trace_agent: FakeTraceAgent) -> None:
    got: list[Any] = []
    writer = writer_for(trace_agent, on_rates=got.append)
    trace_agent.response = {"rate_by_service": {"service:a,env:b": 0.5}}
    writer.submit([span(1)])
    writer.flush()
    assert got == [{"service:a,env:b": 0.5}]
    writer.close()


def test_flushes_on_the_timer(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent, flush_interval=0.05)
    writer.submit([span(1)])
    assert trace_agent.wait_for_chunks(1)
    writer.close()


def test_flushes_early_when_enough_chunks_are_queued(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent, flush_chunks=3)  # the timer is an hour away
    writer.submit([span(1)])
    writer.submit([span(2)])
    time.sleep(0.15)
    assert trace_agent.chunks() == []  # below the threshold: waits for the timer
    writer.submit([span(3)])
    assert trace_agent.wait_for_chunks(3)
    writer.close()


def test_a_body_never_exceeds_the_chunk_or_size_limits(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent, max_queue=2500)
    for i in range(2500):
        writer.submit([span(i)])
    writer.flush()
    assert [len(b["traces"]) for b in trace_agent.bodies] == [1000, 1000, 500]

    trace_agent.bodies.clear()
    trace_agent.raw.clear()
    big = "x" * (3 * 1024 * 1024)
    for i in range(4):
        writer.submit([span(i, resource=big)])
    writer.flush()
    assert len(trace_agent.bodies) == 2  # 3 MiB x 4 does not fit in one 8 MiB body
    assert all(len(raw) <= MAX_BODY_BYTES + 1000 for raw in trace_agent.raw)
    writer.close()


def test_one_oversized_chunk_is_dropped_and_the_rest_survive(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    writer.submit([span(1, resource="x" * (MAX_BODY_BYTES + 10))])
    writer.submit([span(2)])
    writer.flush()
    assert [c[0]["name"] for c in trace_agent.chunks()] == ["n2"]
    assert writer.chunks_dropped == 1
    writer.close()


def test_an_unencodable_chunk_is_dropped_and_counted(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    writer.submit([span(1, metrics={"bad": float("nan")})])
    writer.submit([span(2, meta={"x": object()})])
    writer.submit([span(3)])
    writer.flush()
    assert [c[0]["name"] for c in trace_agent.chunks()] == ["n3"]
    assert writer.chunks_dropped == 2
    writer.close()


def test_empty_chunks_and_submits_after_close_are_ignored(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    writer.submit([])
    assert writer.queued == 0
    writer.close()
    writer.submit([span(1)])
    assert writer.queued == 0


def test_close_drains_the_queue(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    writer.submit([span(1)])
    writer.close()
    assert [c[0]["name"] for c in trace_agent.chunks()] == ["n1"]
    assert writer.chunks_sent == 1


def test_close_is_bounded_by_its_budget_with_a_hung_agent(trace_agent: FakeTraceAgent) -> None:
    trace_agent.delay = 5.0
    writer = writer_for(trace_agent, timeout=4.0)
    writer.submit([span(1)])
    started = time.perf_counter()
    writer.close(0.3)
    assert time.perf_counter() - started < 1.5  # exit is never held hostage by a dead agent
    # The flusher thread is a daemon: it can still be inside its request, and is not joined.


def test_close_with_no_thread_flushes_inline(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    writer._queue.append([span(1)])  # queued without ever starting a thread
    writer.close()
    assert len(trace_agent.chunks()) == 1


def test_the_flusher_thread_is_a_daemon_started_lazily(trace_agent: FakeTraceAgent) -> None:
    writer = writer_for(trace_agent)
    assert writer._thread is None
    writer.submit([span(1)])
    assert writer._thread is not None
    assert writer._thread.daemon
    assert writer._thread.name == "ozy-trace-flusher"
    thread = writer._thread
    writer.close()
    assert thread is not None
    assert not thread.is_alive()  # close() stops and joins this writer's own thread


def test_stats_through_the_tracer_after_drops(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    trace_agent.status = 500
    tracer = make_tracer()
    with tracer.trace("a"):
        pass
    tracer.flush()
    stats = tracer.stats()
    assert (stats.chunks_sent, stats.chunks_dropped, stats.send_errors) == (0, 1, 1)


# -- fork --------------------------------------------------------------------------

forks = pytest.mark.skipif(not hasattr(os, "fork"), reason="platform has no fork()")


@forks
def test_a_forked_child_gets_its_own_flusher_and_none_of_the_parents_queue(
    make_tracer: TracerFactory, trace_agent: FakeTraceAgent
) -> None:
    tracer: Tracer = make_tracer()
    writer = tracer._writer
    assert writer is not None
    with tracer.trace("parent-before-fork"):
        pass
    assert writer.queued == 1  # queued, unsent: the parent still owns this chunk
    parent_thread = writer._thread
    assert parent_thread is not None

    # Fork at the worst moment: the queue lock is held.
    with writer._lock, warnings.catch_warnings():
        warnings.simplefilter("ignore", DeprecationWarning)
        pid = os.fork()

    if pid == 0:  # child
        status = 1
        try:
            ok = writer._thread is None and writer.queued == 0 and writer._lock.acquire(timeout=1)
            if ok:
                writer._lock.release()
                with tracer.trace("child-trace"):
                    pass
                ok = writer._thread is not None and writer._thread is not parent_thread
                tracer.flush()
                ok = ok and tracer.stats().chunks_sent == 1
            status = 0 if ok else 1
        finally:
            os._exit(status)

    _, wait_status = os.waitpid(pid, 0)
    assert os.waitstatus_to_exitcode(wait_status) == 0
    tracer.flush()
    names = sorted(c[0]["name"] for c in trace_agent.chunks())
    # Each chunk exactly once: the child did not re-send the parent's.
    assert names == ["child-trace", "parent-before-fork"]


@forks
def test_the_exit_hook_is_registered_once_per_tracer(
    monkeypatch: pytest.MonkeyPatch, trace_agent: FakeTraceAgent
) -> None:
    import atexit

    calls: list[Any] = []
    monkeypatch.setattr(atexit, "register", lambda fn, *a: calls.append(fn))
    monkeypatch.setattr(os, "register_at_fork", lambda **kw: calls.append(kw))
    from ozy import Config

    tracer = Tracer()
    config = Config(agent_host="127.0.0.1", trace_port=trace_agent.port)
    tracer.configure(config)
    tracer.configure(config)  # init() twice must not stack a second hook
    assert len(calls) == 2  # one atexit + one at-fork
    tracer.close()
