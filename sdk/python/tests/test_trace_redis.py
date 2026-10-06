"""Redis integration through a stub connection: command name only, never keys or values."""

from __future__ import annotations

import asyncio
from typing import Any, ClassVar

import pytest
import redis
import redis.asyncio as aredis
import redis.asyncio.client as aclient
from redis.asyncio.connection import Connection as AsyncConnection
from redis.asyncio.connection import ConnectionPool as AsyncPool
from redis.connection import Connection as SyncConnection
from redis.connection import ConnectionPool as SyncPool

import ozy
from ozy.integrations import redis as ozy_redis
from ozy.integrations.redis import INTEGRATION

from .conftest import FakeTraceAgent

KEY = "session:sentinel-key-4471"
VALUE = "sentinel-value-9082"


class AsyncStub(AsyncConnection):
    sent: ClassVar[list[Any]] = []
    responses: ClassVar[list[Any]] = []

    async def connect(self) -> None:
        pass

    @property
    def is_connected(self) -> bool:
        return True

    async def can_read_destructive(self) -> bool:
        return False

    async def disconnect(self, *a: Any, **k: Any) -> None:
        pass

    async def send_command(self, *args: Any, **kw: Any) -> None:
        AsyncStub.sent.append(args)

    async def send_packed_command(self, command: Any, check_health: bool = True) -> None:
        AsyncStub.sent.append(("packed", command))

    async def read_response(self, *a: Any, **k: Any) -> Any:
        reply = AsyncStub.responses.pop(0) if AsyncStub.responses else "OK"
        if isinstance(reply, Exception):
            raise reply
        return reply


class SyncStub(SyncConnection):
    responses: ClassVar[list[Any]] = []

    def connect(self) -> None:
        pass

    @property
    def is_connected(self) -> bool:
        return True

    def can_read(self, timeout: float = 0) -> bool:
        return False

    def disconnect(self, *a: Any, **k: Any) -> None:
        pass

    def send_command(self, *args: Any, **kw: Any) -> None:
        pass

    def send_packed_command(self, command: Any, check_health: bool = True) -> None:
        pass

    def read_response(self, *a: Any, **k: Any) -> Any:
        reply = SyncStub.responses.pop(0) if SyncStub.responses else "OK"
        if isinstance(reply, Exception):
            raise reply
        return reply


@pytest.fixture(autouse=True)
def reset_stubs() -> None:
    AsyncStub.sent, AsyncStub.responses, SyncStub.responses = [], [], []


def redis_spans(agent: FakeTraceAgent) -> list[dict[str, Any]]:
    ozy.tracer.flush()
    return [s for s in agent.spans() if s["name"] == "redis.command"]


def async_client() -> aredis.Redis:
    return aredis.Redis(connection_pool=AsyncPool(connection_class=AsyncStub))


def sync_client() -> redis.Redis:
    return redis.Redis(connection_pool=SyncPool(connection_class=SyncStub))


def test_an_async_command_inside_a_trace_is_a_cache_span_named_by_the_command(
    traced: FakeTraceAgent,
) -> None:
    INTEGRATION.patch()

    async def run() -> Any:
        client = async_client()
        async with ozy.tracer.trace("request") as root:
            await client.set(KEY, VALUE)
            return root

    root = asyncio.run(run())
    (span,) = redis_spans(traced)
    assert span["resource"] == "SET"
    assert span["type"] == "cache"
    assert span["parent_id"] == root.span_id
    assert span["meta"]["db.system"] == "redis"
    assert span["meta"]["span.kind"] == "client"
    assert "_top_level" not in span["metrics"]
    assert ("SET", KEY, VALUE) in AsyncStub.sent  # the call really reached the connection


def test_neither_keys_nor_values_reach_the_payload(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    async def run() -> None:
        client = async_client()
        async with ozy.tracer.trace("request"):
            await client.set(KEY, VALUE)
            await client.get(KEY)
            await client.hset("h:" + KEY, VALUE, VALUE)  # type: ignore[misc]
            async with client.pipeline(transaction=False) as pipe:
                pipe.set(KEY, VALUE)
                pipe.get(KEY)
                await pipe.execute()
            AsyncStub.responses.append(redis.ResponseError(f"ERR unknown command, args: {KEY}"))
            execute: Any = client.execute_command
            with pytest.raises(redis.ResponseError):
                await execute("NOPE", KEY)

    asyncio.run(run())
    ozy.tracer.flush()
    assert traced.raw
    assert all(KEY.encode() not in b and VALUE.encode() not in b for b in traced.raw)
    assert {s["resource"] for s in redis_spans(traced)} == {
        "SET",
        "GET",
        "HSET",
        "PIPELINE",
        "NOPE",
    }


def test_a_failure_records_the_exception_type_only_and_reraises(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    async def run() -> None:
        client = async_client()
        async with ozy.tracer.trace("request"):
            AsyncStub.responses.append(redis.ResponseError(f"ERR boom {KEY}"))
            with pytest.raises(redis.ResponseError, match="boom"):
                await client.get(KEY)

    asyncio.run(run())
    (span,) = redis_spans(traced)
    assert span["error"] == 1
    assert span["meta"]["error.type"] == "redis.exceptions.ResponseError"
    assert "error.message" not in span["meta"]
    assert "error.stack" not in span["meta"]


def test_a_pipeline_is_one_span_with_a_command_count(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    async def run() -> None:
        client = async_client()
        async with ozy.tracer.trace("request"), client.pipeline(transaction=False) as pipe:
            pipe.set("a", "1")
            pipe.set("b", "2")
            pipe.get("a")
            await pipe.execute()

    asyncio.run(run())
    pipelines = [s for s in redis_spans(traced) if s["resource"] == "PIPELINE"]
    assert len(pipelines) == 1
    assert pipelines[0]["metrics"]["redis.pipeline.commands"] == 3


def test_the_sync_client_is_traced_too(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    client = sync_client()
    with ozy.tracer.trace("request") as root:
        client.set(KEY, VALUE)
        with client.pipeline(transaction=False) as pipe:
            pipe.get(KEY)
            pipe.execute()
        SyncStub.responses.append(redis.ConnectionError("down"))
        with pytest.raises(redis.ConnectionError):
            client.get(KEY)
    spans = redis_spans(traced)
    assert [s["resource"] for s in spans] == ["SET", "PIPELINE", "GET"]
    assert {s["parent_id"] for s in spans} == {root.span_id}
    assert [s["error"] for s in spans] == [0, 0, 1]
    assert all(KEY.encode() not in b for b in traced.raw)


def test_commands_outside_a_trace_are_not_traced(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    async def run() -> None:
        client = async_client()
        await client.get("x")
        async with client.pipeline(transaction=False) as pipe:
            pipe.get("x")
            await pipe.execute()

    asyncio.run(run())
    sync_client().get("x")
    assert redis_spans(traced) == []  # arq's own queue polling is the typical case


def test_patch_is_idempotent_and_unpatch_restores_the_methods(traced: FakeTraceAgent) -> None:
    original = aredis.Redis.execute_command
    pipeline_queueing = vars(aclient.Pipeline)["execute_command"]  # queues, never talks to redis
    INTEGRATION.patch()
    INTEGRATION.patch()
    patched = aredis.Redis.execute_command
    assert patched.__wrapped__ is original  # type: ignore[attr-defined]  # wrapped exactly once
    INTEGRATION.unpatch()
    assert aredis.Redis.execute_command is original
    assert vars(aclient.Pipeline)["execute_command"] is pipeline_queueing  # never touched
    INTEGRATION.unpatch()


def test_command_names_are_normalized_and_bounded() -> None:
    assert ozy_redis._command_name(("set", "k")) == "SET"
    assert ozy_redis._command_name((b"hget", "k")) == "HGET"
    assert ozy_redis._command_name(("CLIENT SETNAME", "x")) == "CLIENT"
    assert ozy_redis._command_name(("x" * 500,)) == "X" * 32
    assert ozy_redis._command_name(()) == "UNKNOWN"
    assert ozy_redis._command_name((None,)) == "NONE"
    assert ozy_redis._command_name((b"\xff\xfe",)) != ""


def test_unavailable_library_is_a_noop(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real = builtins.__import__

    def fake(name: str, *a: Any, **k: Any) -> Any:
        if name.startswith("redis"):
            raise ImportError(name)
        return real(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", fake)
    assert INTEGRATION.is_available() is False
    INTEGRATION.patch()
