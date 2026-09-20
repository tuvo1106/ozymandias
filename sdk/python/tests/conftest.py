"""Shared fixtures: a real UDP fake agent and self-cleaning clients."""

from __future__ import annotations

import os
import socket
import time
from collections.abc import Callable, Iterator
from typing import Any

import pytest

import ozy
from ozy import Config, StatsdClient


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
