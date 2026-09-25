"""L10 safety suite (statsd parts): the SDK must never hurt the host app."""

from __future__ import annotations

import atexit
import os
import socket
import threading
import time
from typing import Any

import pytest

import ozy
from ozy import Config, StatsdClient, statsd

from .conftest import ClientFactory, FakeAgent, eventually


def exercise_everything(client: StatsdClient) -> None:
    client.increment("a", tags=["x:y"])
    client.decrement("a")
    client.gauge("b", 1)
    client.histogram("c", 1)
    client.distribution("d", 1)
    client.timing("e", 1)
    client.set("f", "member")
    with client.timed("g"):
        pass
    client.timed("h")(lambda: None)()
    client.flush()
    client.close()


def test_disabled_creates_no_socket_thread_or_hooks(monkeypatch: pytest.MonkeyPatch) -> None:
    created: list[Any] = []

    def spy(*args: Any, **kwargs: Any) -> Any:
        created.append(("call", args))
        raise AssertionError("must not be called while disabled")

    monkeypatch.setattr(socket, "socket", spy)
    monkeypatch.setattr(socket, "getaddrinfo", spy)
    monkeypatch.setattr(atexit, "register", spy)
    monkeypatch.setattr(os, "register_at_fork", spy)
    threads_before = set(threading.enumerate())

    ozy.init(service="svc")  # no OZY_AGENT_HOST, no agent_host
    assert not statsd.enabled
    exercise_everything(statsd)

    assert created == []
    assert set(threading.enumerate()) == threads_before
    assert statsd.stats() == ozy.Stats()


def test_explicitly_empty_host_disables(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OZY_AGENT_HOST", "127.0.0.1")
    ozy.init(agent_host="")
    assert not statsd.enabled


def test_closed_port_never_raises_and_counts_errors(make_client: ClientFactory) -> None:
    # Bind then close to get a port with (almost certainly) no listener.
    probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()

    client = make_client(statsd_port=port)
    for _ in range(5):
        client.increment("into.the.void")
        client.flush()  # must not raise
        time.sleep(0.02)  # let the ICMP "port unreachable" come back
    stats = client.stats()
    # The first send succeeds locally; later ones see ECONNREFUSED on a
    # connected UDP socket (Linux and macOS both report it).
    assert stats.sent + stats.dropped == 5
    assert stats.errors >= 1
    assert stats.errors == stats.dropped


def test_dns_failure_never_raises_and_counts_errors(make_client: ClientFactory) -> None:
    calls = []

    def failing_resolver(*args: Any) -> list[tuple[Any, ...]]:
        calls.append(args)
        raise socket.gaierror(socket.EAI_NONAME, "nodename nor servname provided")

    client = make_client(agent_host="agent.invalid", resolver=failing_resolver)
    client.increment("a")
    client.flush()
    client.increment("b")
    client.flush()
    assert client.stats() == ozy.Stats(sent=0, dropped=2, errors=2)
    assert len(calls) == 1  # the failure is cached, not retried per send


def test_resolver_returning_nothing_is_an_error(make_client: ClientFactory) -> None:
    client = make_client(resolver=lambda *a: [])
    client.increment("a")
    client.flush()
    assert client.stats().errors == 1


def test_socket_creation_failure_is_counted(make_client: ClientFactory) -> None:
    def broken_factory(family: int, kind: int) -> socket.socket:
        raise OSError("too many open files")

    client = make_client(socket_factory=broken_factory)
    client.increment("a")
    client.flush()
    assert client.stats() == ozy.Stats(sent=0, dropped=1, errors=1)


def test_connect_failure_closes_the_new_socket(make_client: ClientFactory) -> None:
    closed = []

    class BadSocket:
        def setblocking(self, flag: bool) -> None:
            pass

        def connect(self, addr: Any) -> None:
            raise OSError("connect failed")

        def close(self) -> None:
            closed.append(True)

    client = make_client(socket_factory=lambda f, t: BadSocket())
    client.increment("a")
    client.flush()
    assert closed == [True]
    assert client.stats().errors == 1


def test_unexpected_send_error_rebuilds_socket(make_client: ClientFactory) -> None:
    sockets: list[Any] = []

    class FlakySocket:
        def __init__(self) -> None:
            self.closed = False
            sockets.append(self)

        def setblocking(self, flag: bool) -> None:
            pass

        def connect(self, addr: Any) -> None:
            pass

        def send(self, data: bytes) -> int:
            if len(sockets) == 1:
                raise OSError("EBADF")
            return len(data)

        def close(self) -> None:
            self.closed = True

    client = make_client(socket_factory=lambda f, t: FlakySocket())
    client.increment("a")
    client.flush()
    client.increment("b")
    client.flush()
    assert len(sockets) == 2
    assert sockets[0].closed
    assert client.stats() == ozy.Stats(sent=1, packets=1, dropped=1, errors=1)


def test_full_socket_buffer_drops_and_keeps_socket(make_client: ClientFactory) -> None:
    sockets: list[Any] = []

    class FullSocket:
        def __init__(self) -> None:
            sockets.append(self)

        def setblocking(self, flag: bool) -> None:
            assert flag is False

        def connect(self, addr: Any) -> None:
            pass

        def send(self, data: bytes) -> int:
            raise BlockingIOError("would block")

        def close(self) -> None:
            pass

    client = make_client(socket_factory=lambda f, t: FullSocket())
    for _ in range(3):
        client.increment("a")
        client.flush()
    assert len(sockets) == 1
    assert client.stats() == ozy.Stats(sent=0, dropped=3, errors=3)


def test_calls_return_fast_when_agent_is_stuck(make_client: ClientFactory) -> None:
    # The sender is blocked inside a send: callers only enqueue.
    client = make_client(max_payload=16)
    with client._send_lock:
        start = time.perf_counter()
        for i in range(1000):
            client.increment(f"m{i}")
        per_call = (time.perf_counter() - start) / 1000
    assert per_call < 0.001


@pytest.mark.parametrize(
    "bad_call",
    [
        lambda c: c.gauge("g", "not a number"),
        lambda c: c.gauge("g", None),
        lambda c: c.increment("g", object()),
        lambda c: c.decrement("g", "x"),
        lambda c: c.histogram("g", 10**400),
    ],
)
def test_bad_values_are_dropped_not_raised(
    make_client: ClientFactory, agent: FakeAgent, bad_call: Any
) -> None:
    client = make_client()
    bad_call(client)
    client.flush()
    assert client.stats().dropped == 1
    assert agent.recv_or_none(0.05) is None


def test_internal_errors_are_swallowed_and_counted(make_client: ClientFactory) -> None:
    client = make_client()
    client.increment(None)  # type: ignore[arg-type]
    client.gauge("g", 1, tags=[object()])  # type: ignore[list-item]
    assert client.stats().errors == 2


def test_debug_logs_errors(make_client: ClientFactory, caplog: pytest.LogCaptureFixture) -> None:
    def failing_resolver(*args: Any) -> list[tuple[Any, ...]]:
        raise socket.gaierror("boom")

    client = make_client(debug=True, resolver=failing_resolver)
    with caplog.at_level("DEBUG", logger="ozy"):
        client.increment("a")
        client.flush()
    assert any("failed" in r.getMessage() for r in caplog.records)


def test_debug_logs_payloads(
    make_client: ClientFactory, agent: FakeAgent, caplog: pytest.LogCaptureFixture
) -> None:
    client = make_client(debug=True)
    with caplog.at_level("DEBUG", logger="ozy"):
        client.increment("a")
        client.flush()
    agent.recv()
    assert any("a:1|c" in r.getMessage() for r in caplog.records)


def test_keyboard_interrupt_is_not_swallowed(make_client: ClientFactory) -> None:
    def interrupting_random() -> float:
        raise KeyboardInterrupt

    client = make_client(random=interrupting_random)
    with pytest.raises(KeyboardInterrupt):
        client.increment("a", sample_rate=0.5)


def test_init_failure_leaves_sdk_disabled(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    def boom(*args: Any, **kwargs: Any) -> Any:
        raise RuntimeError("config exploded")

    monkeypatch.setattr(ozy, "resolve_config", boom)
    ozy.init(agent_host="127.0.0.1")  # must not raise
    assert not statsd.enabled
    assert "init failed" in caplog.text


def test_close_from_flusher_thread_does_not_deadlock(make_client: ClientFactory) -> None:
    client = make_client(flush_interval=0.01)
    client.increment("a")
    assert client._thread is not None
    # close() must not try to join the thread it is running on.
    done = threading.Event()
    client._thread = threading.current_thread()
    client.close()
    done.set()
    assert done.is_set()
    assert eventually(lambda: not client.enabled)


class TestFailuresStayInsideTheSdk:
    """Regressions from the M1 review: three ways a send failure escaped."""

    def test_malformed_hostname_raises_oserror_not_assertionerror(
        self, make_client: ClientFactory
    ) -> None:
        # getaddrinfo raises UnicodeError, not OSError, for a DNS label that
        # is empty or over 63 characters. Catching only OSError left the
        # resolver marked "resolved" with no address, so every send for the
        # next 60 s hit an assert instead of the documented OSError.
        client = make_client(agent_host="agent..local")
        client.increment("nope")
        client.flush()
        stats = client.stats()
        assert stats.dropped == 1, stats
        assert stats.errors >= 1, stats
        assert stats.sent == 0, stats

    def test_a_non_oserror_send_failure_is_counted_and_does_not_abandon_the_rest(
        self, make_client: ClientFactory, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        client = make_client()
        client.increment("first")
        client.flush()
        assert client.stats().sent == 1

        def boom(_payload: bytes) -> None:
            raise RuntimeError("not an OSError")

        assert client._transport is not None
        monkeypatch.setattr(client._transport, "send", boom)
        client.increment("second")
        client.increment("third")
        client.flush()

        stats = client.stats()
        assert stats.dropped == 2, f"lost messages must be counted: {stats}"
        assert stats.sent == 1, stats

    def test_close_still_closes_the_socket_when_the_final_flush_fails(
        self, make_client: ClientFactory, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        # close() flushes first. When that raised, @_safe swallowed it and the
        # socket was never closed while the client already looked shut down.
        client = make_client()
        client.increment("x")
        transport = client._transport
        assert transport is not None

        def boom(_payload: bytes) -> None:
            raise RuntimeError("boom")

        monkeypatch.setattr(transport, "send", boom)
        client.close()
        assert transport._sock is None, "the socket outlived close()"

    def test_the_flusher_thread_does_not_keep_a_discarded_client_alive(
        self, agent: FakeAgent
    ) -> None:
        # The thread used to be started on a bound method, which is a strong
        # reference for as long as the thread runs — i.e. forever.
        import gc
        import weakref

        client = StatsdClient()
        client.configure(
            Config(agent_host="127.0.0.1", statsd_port=agent.port, flush_interval=60.0)
        )
        client.increment("start.the.thread")
        ref = weakref.ref(client)
        del client
        gc.collect()
        assert ref() is None, "the flusher thread is still holding the client"
