"""Shared fixtures: a real UDP fake agent and self-cleaning clients."""

from __future__ import annotations

import contextlib
import json
import os
import socket
import threading
import time
from collections.abc import Callable, Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

import ozy
from ozy import Config, StatsdClient, Tracer


class FakeAgent:
    """A UDP socket on 127.0.0.1:<ephemeral> standing in for the agent."""

    def __init__(self) -> None:
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.bind(("127.0.0.1", 0))
        self.port: int = self.sock.getsockname()[1]

    def recv(self, timeout: float = 2.0) -> str:
        """Return the next datagram, failing the test if none arrives in time."""
        self.sock.settimeout(timeout)
        try:
            data, _ = self.sock.recvfrom(65535)
        except TimeoutError:
            pytest.fail(f"no datagram within {timeout}s")
        return data.decode("utf-8")

    def recv_or_none(self, timeout: float) -> str | None:
        """Return the next datagram, or None if none arrives in time."""
        self.sock.settimeout(timeout)
        try:
            data, _ = self.sock.recvfrom(65535)
        except TimeoutError:
            return None
        return data.decode("utf-8")

    def close(self) -> None:
        self.sock.close()


@pytest.fixture(autouse=True)
def clean_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """No test sees the developer's OZY_* variables."""
    for key in list(os.environ):
        if key.startswith("OZY_"):
            monkeypatch.delenv(key)


@pytest.fixture(autouse=True)
def reset_global_client() -> Iterator[None]:
    """Leave the process-wide ``ozy.statsd`` disabled after every test."""
    yield
    ozy.statsd.configure(Config())


@pytest.fixture(autouse=True)
def reset_global_tracer() -> Iterator[None]:
    """Leave the process-wide ``ozy.tracer`` disabled and every integration unpatched."""
    yield
    from ozy._tracer import _current
    from ozy.integrations import _BUILTIN, _registry, unpatch_all

    _current.set(None)
    unpatch_all()
    for name in [n for n in _registry if n not in _BUILTIN]:
        del _registry[name]  # third-party registrations made by a test do not outlive it
    ozy.tracer.configure(Config())


class FakeTraceAgent:
    """A real HTTP server on 127.0.0.1:<ephemeral> standing in for the agent's trace receiver.

    Records every body it is sent; ``status``, ``response`` and ``delay`` can be changed
    between requests to play a down, busy or slow agent.
    """

    def __init__(self) -> None:
        self.bodies: list[dict[str, Any]] = []
        self.raw: list[bytes] = []
        self.paths: list[str] = []
        self.status = 200
        self.response: dict[str, Any] = {"rate_by_service": {}, "accepted": 0, "rejected": 0}
        self.delay = 0.0
        self._lock = threading.Lock()
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self) -> None:
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                if outer.delay:
                    time.sleep(outer.delay)
                with outer._lock:
                    outer.raw.append(body)
                    outer.paths.append(self.path)
                    with contextlib.suppress(ValueError):
                        outer.bodies.append(json.loads(body))
                payload = json.dumps(outer.response).encode()
                self.send_response(outer.status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *args: Any) -> None:
                pass

        class QuietServer(ThreadingHTTPServer):
            def handle_error(self, request: Any, client_address: Any) -> None:
                pass  # a client that timed out and hung up is a scenario here, not a traceback

        self.server = QuietServer(("127.0.0.1", 0), Handler)
        self.port: int = self.server.server_address[1]
        self._thread = threading.Thread(
            target=self.server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True
        )
        self._thread.start()

    def chunks(self) -> list[list[dict[str, Any]]]:
        """Every chunk received so far, in arrival order."""
        with self._lock:
            return [chunk for body in self.bodies for chunk in body["traces"]]

    def spans(self) -> list[dict[str, Any]]:
        """Every span received so far, flattened."""
        return [span for chunk in self.chunks() for span in chunk]

    def wait_for_chunks(self, count: int, timeout: float = 3.0) -> bool:
        """Poll until at least ``count`` chunks arrived."""
        return eventually(lambda: len(self.chunks()) >= count, timeout)

    def close(self) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def trace_agent() -> Iterator[FakeTraceAgent]:
    fake = FakeTraceAgent()
    yield fake
    fake.close()


TracerFactory = Callable[..., Tracer]


@pytest.fixture
def make_tracer(trace_agent: FakeTraceAgent) -> Iterator[TracerFactory]:
    """Build an enabled, *private* tracer pointed at the fake trace agent; closed at teardown.

    Keyword arguments go to ``Config`` except ``wall_ns``/``mono_ns``/``random_bytes``,
    which go to the ``Tracer`` constructor.
    """
    tracers: list[Tracer] = []
    ctor_keys = {"wall_ns", "mono_ns", "random_bytes"}

    def factory(**kwargs: Any) -> Tracer:
        ctor = {k: kwargs.pop(k) for k in list(kwargs) if k in ctor_keys}
        settings: dict[str, Any] = {
            "agent_host": "127.0.0.1",
            "trace_port": trace_agent.port,
            "service": "svc",
            "env": "test",
        }
        settings.update(kwargs)
        tracer = Tracer(**ctor)
        tracer.configure(Config(**settings))
        tracers.append(tracer)
        return tracer

    yield factory
    for tracer in tracers:
        tracer.close()


@pytest.fixture
def traced(trace_agent: FakeTraceAgent) -> FakeTraceAgent:
    """Enable the *global* ``ozy.tracer`` against the fake agent (integrations use it)."""
    ozy.tracer.configure(
        Config(agent_host="127.0.0.1", trace_port=trace_agent.port, service="svc", env="test")
    )
    return trace_agent


@pytest.fixture
def agent() -> Iterator[FakeAgent]:
    fake = FakeAgent()
    yield fake
    fake.close()


ClientFactory = Callable[..., StatsdClient]


@pytest.fixture
def make_client(agent: FakeAgent) -> Iterator[ClientFactory]:
    """Build an enabled client pointed at the fake agent; closed at teardown.

    Keyword arguments go to ``Config`` (overriding the fake agent's address
    and a long flush interval, so tests control flushing explicitly), except
    ``random``/``clock``/``resolver``/``socket_factory``, which go to the
    client constructor.
    """
    clients: list[StatsdClient] = []
    ctor_keys = {"random", "clock", "resolver", "socket_factory"}

    def factory(**kwargs: Any) -> StatsdClient:
        ctor = {k: kwargs.pop(k) for k in list(kwargs) if k in ctor_keys}
        settings: dict[str, Any] = {
            "agent_host": "127.0.0.1",
            "statsd_port": agent.port,
            "flush_interval": 60.0,
        }
        settings.update(kwargs)
        client = StatsdClient(**ctor)
        client.configure(Config(**settings))
        clients.append(client)
        return client

    yield factory
    for client in clients:
        client.close()


def eventually(predicate: Callable[[], bool], timeout: float = 2.0) -> bool:
    """Poll ``predicate`` until true or ``timeout``; returns the last result."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(0.01)
    return predicate()
