"""``statsd.timed``: context manager and decorator, sync and async, errors propagate."""

from __future__ import annotations

import asyncio
import inspect

import pytest

from ozy import StatsdClient

from .conftest import ClientFactory, FakeAgent


class FakeClock:
    """Advances by a fixed step on every read, so durations are exact."""

    def __init__(self, step: float = 0.25) -> None:
        self.now = 100.0
        self.step = step

    def __call__(self) -> float:
        self.now += self.step
        return self.now


@pytest.fixture
def timed_client(make_client: ClientFactory) -> StatsdClient:
    # Every clock read advances 0.25 s, so enter->exit is exactly 250 ms.
    return make_client(clock=FakeClock())


def sent(client: StatsdClient, agent: FakeAgent) -> str:
    client.flush()
    return agent.recv()


class Boom(Exception):
    pass


def test_context_manager(timed_client: StatsdClient, agent: FakeAgent) -> None:
    with timed_client.timed("block.duration", tags=["k:v"]):
        pass
    assert sent(timed_client, agent) == "block.duration:250|ms|#k:v"


def test_context_manager_reraises_unchanged_and_records(
    timed_client: StatsdClient, agent: FakeAgent
) -> None:
    err = Boom("original")
    with pytest.raises(Boom) as info, timed_client.timed("block.duration"):
        raise err
    assert info.value is err
    assert sent(timed_client, agent) == "block.duration:250|ms"


def test_nested_use_of_one_timer_object(timed_client: StatsdClient, agent: FakeAgent) -> None:
    timer = timed_client.timed("nested")
    with timer, timer:
        pass
    # inner: enter@t1, exit@t2 (250ms); outer: enter@t0, exit@t3 (750ms)
    assert sent(timed_client, agent) == "nested:250|ms\nnested:750|ms"


def test_sync_decorator(timed_client: StatsdClient, agent: FakeAgent) -> None:
    @timed_client.timed("fn.duration")
    def add(a: int, b: int) -> int:
        """Adds."""
        return a + b

    assert add(2, 3) == 5
    assert add.__name__ == "add"
    assert add.__doc__ == "Adds."
    assert sent(timed_client, agent) == "fn.duration:250|ms"


def test_sync_decorator_reraises_unchanged_and_records(
    timed_client: StatsdClient, agent: FakeAgent
) -> None:
    err = Boom("sync")

    @timed_client.timed("fn.duration")
    def fail() -> None:
        raise err

    with pytest.raises(Boom) as info:
        fail()
    assert info.value is err
    assert sent(timed_client, agent) == "fn.duration:250|ms"


def test_async_decorator(timed_client: StatsdClient, agent: FakeAgent) -> None:
    @timed_client.timed("coro.duration")
    async def work(x: int) -> int:
        await asyncio.sleep(0)
        return x * 2

    assert inspect.iscoroutinefunction(work)
    assert asyncio.run(work(21)) == 42
    assert sent(timed_client, agent) == "coro.duration:250|ms"


def test_async_decorator_reraises_unchanged_and_records(
    timed_client: StatsdClient, agent: FakeAgent
) -> None:
    err = Boom("async")

    @timed_client.timed("coro.duration")
    async def fail() -> None:
        await asyncio.sleep(0)
        raise err

    with pytest.raises(Boom) as info:
        asyncio.run(fail())
    assert info.value is err
    assert sent(timed_client, agent) == "coro.duration:250|ms"


def test_async_context_manager(timed_client: StatsdClient, agent: FakeAgent) -> None:
    async def main() -> None:
        async with timed_client.timed("actx.duration"):
            await asyncio.sleep(0)

    asyncio.run(main())
    assert sent(timed_client, agent) == "actx.duration:250|ms"


def test_real_clock_records_a_plausible_duration(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    with client.timed("real"):
        pass
    name, rest = sent(client, agent).split(":", 1)
    value, kind = rest.split("|")
    assert (name, kind) == ("real", "ms")
    assert 0 <= float(value) < 1000


def test_broken_clock_never_raises(make_client: ClientFactory) -> None:
    def broken() -> float:
        raise RuntimeError("clock")

    client = make_client(clock=broken)
    with client.timed("x"):
        pass

    @client.timed("y")
    def f() -> int:
        return 1

    assert f() == 1
    assert client.stats().errors >= 2


def test_timed_is_noop_when_disabled() -> None:
    client = StatsdClient()
    with client.timed("x"):
        pass
    assert client.timed("y")(lambda: 7)() == 7
    assert client.stats().sent == 0


def test_exit_without_enter_is_tolerated(timed_client: StatsdClient, agent: FakeAgent) -> None:
    timer = timed_client.timed("odd")
    timer.__exit__(None, None, None)
    assert sent(timed_client, agent) == "odd:250|ms"
