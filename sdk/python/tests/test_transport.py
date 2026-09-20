"""UdpTransport: DNS cached for 60 s, socket rebuilt when the address changes."""

from __future__ import annotations

import contextlib
import socket
from typing import Any

import pytest

from ozy._transport import DNS_TTL_SECONDS, UdpTransport

from .conftest import FakeAgent


class ManualClock:
    def __init__(self) -> None:
        self.now = 0.0

    def __call__(self) -> float:
        return self.now


def test_dns_resolved_at_most_once_per_ttl(agent: FakeAgent) -> None:
    clock = ManualClock()
    calls: list[tuple[Any, ...]] = []

    def resolver(host: str, port: int, *rest: Any) -> list[tuple[Any, ...]]:
        calls.append((host, port))
        return socket.getaddrinfo("127.0.0.1", port, 0, socket.SOCK_DGRAM)

    t = UdpTransport("agent.example", agent.port, clock=clock, resolver=resolver)
    for _ in range(3):
        t.send(b"a:1|c")
    clock.now = DNS_TTL_SECONDS - 0.001
    t.send(b"a:1|c")
    assert len(calls) == 1
    clock.now = DNS_TTL_SECONDS
    t.send(b"a:1|c")
    assert len(calls) == 2
    assert [agent.recv() for _ in range(5)] == ["a:1|c"] * 5
    t.close()
    t.close()  # idempotent


def test_dns_failure_is_cached_then_retried(agent: FakeAgent) -> None:
    clock = ManualClock()
    state = {"fail": True, "calls": 0}

    def resolver(host: str, port: int, *rest: Any) -> list[tuple[Any, ...]]:
        state["calls"] += 1
        if state["fail"]:
            raise socket.gaierror("temporary failure")
        return socket.getaddrinfo("127.0.0.1", port, 0, socket.SOCK_DGRAM)

    t = UdpTransport("agent.example", agent.port, clock=clock, resolver=resolver)
    for _ in range(3):
        with pytest.raises(OSError, match=r"agent\.example"):
            t.send(b"x")
    assert state["calls"] == 1
    state["fail"] = False
    clock.now = DNS_TTL_SECONDS
    t.send(b"recovered:1|c")
    assert agent.recv() == "recovered:1|c"
    t.close()


def test_address_change_opens_new_socket(agent: FakeAgent) -> None:
    other = FakeAgent()
    try:
        clock = ManualClock()
        target = {"port": agent.port}

        def resolver(host: str, port: int, *rest: Any) -> list[tuple[Any, ...]]:
            return socket.getaddrinfo("127.0.0.1", target["port"], 0, socket.SOCK_DGRAM)

        t = UdpTransport("agent.example", 1, clock=clock, resolver=resolver)
        t.send(b"first")
        target["port"] = other.port
        clock.now = DNS_TTL_SECONDS
        t.send(b"second")
        assert agent.recv() == "first"
        assert other.recv() == "second"
        t.close()
    finally:
        other.close()


def test_real_resolver_and_socket_by_default(agent: FakeAgent) -> None:
    # "localhost" may resolve to ::1 first (no listener there), so only
    # require that the real getaddrinfo path raises nothing but OSError.
    t = UdpTransport("localhost", agent.port, clock=ManualClock())
    with contextlib.suppress(OSError):
        t.send(b"via-localhost")
    t.close()
    t2 = UdpTransport("127.0.0.1", agent.port, clock=ManualClock())
    t2.send(b"direct")
    received = {agent.recv(), agent.recv_or_none(0.1)}
    assert "direct" in received
    assert received <= {"direct", "via-localhost", None}
    t2.close()
