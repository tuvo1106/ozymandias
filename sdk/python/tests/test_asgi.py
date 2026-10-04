"""The ASGI middleware: right tags, honest statuses, never harms a request."""

from __future__ import annotations

import asyncio
from collections.abc import Callable, Iterator, MutableMapping
from types import SimpleNamespace
from typing import Any

import pytest

from ozy import Config, StatsdClient
from ozy.integrations.asgi import MetricsMiddleware

from .conftest import ClientFactory, FakeAgent


def run_request(
    app: Callable[..., Any],
    client: StatsdClient,
    *,
    path: str = "/x",
    method: str = "GET",
    scope_type: str = "http",
    **kwargs: Any,
) -> list[MutableMapping[str, Any]]:
    """Drive one request through the middleware; return the messages sent."""
    sent: list[MutableMapping[str, Any]] = []

    async def receive() -> dict[str, Any]:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: MutableMapping[str, Any]) -> None:
        sent.append(message)

    scope: dict[str, Any] = {"type": scope_type, "path": path, "method": method}
    asyncio.run(MetricsMiddleware(app, client=client, **kwargs)(scope, receive, send))
    return sent


def responder(status: int, route: str | None = None) -> Callable[..., Any]:
    async def app(scope: Any, receive: Any, send: Any) -> None:
        if route is not None:
            scope["route"] = SimpleNamespace(path=route)  # what Starlette does
        await send({"type": "http.response.start", "status": status, "headers": []})
        await send({"type": "http.response.body", "body": b"ok"})

    return app


def datagrams(agent: FakeAgent, client: StatsdClient) -> list[str]:
    client.flush()
    return agent.recv().splitlines()


def count_line(agent: FakeAgent, client: StatsdClient) -> str:
    """The counter's datagram line. Unlike the duration's it holds no measured value, so a
    test may search it for digits (``499``, an id) without matching a random millisecond."""
    return next(line for line in datagrams(agent, client) if line.startswith("http.request.count"))


def test_records_count_and_duration_tagged_with_route_pattern(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    run_request(responder(200, "/api/v1/problems/{slug}"), client, path="/api/v1/problems/two-sum")
    lines = datagrams(agent, client)
    tags = "|#route:/api/v1/problems/{slug},method:GET,status:200,status_class:2xx"
    assert f"http.request.count:1|c{tags}" in lines
    duration = next(line for line in lines if line.startswith("http.request.duration:"))
    assert duration.endswith(f"|d{tags}")
    assert "two-sum" not in "".join(lines)  # letters cannot appear in a measured number


def test_unmatched_route_is_one_series(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()
    run_request(responder(404), client, path="/wp-login.php")
    assert "route:unmatched" in "".join(datagrams(agent, client))


def test_raising_app_is_recorded_as_500_and_still_raises(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def boom(scope: Any, receive: Any, send: Any) -> None:
        raise RuntimeError("boom")

    with pytest.raises(RuntimeError, match="boom"):
        run_request(boom, client)
    assert "status:500,status_class:5xx" in "".join(datagrams(agent, client))


def test_error_after_response_started_keeps_the_sent_status(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def late_failure(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        raise RuntimeError("mid-stream")

    with pytest.raises(RuntimeError):
        run_request(late_failure, client)
    assert "status:200" in "".join(datagrams(agent, client))


def test_cancelled_after_the_client_disconnected_is_499_not_500(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def sees_disconnect(scope: Any, receive: Any, send: Any) -> None:
        await receive()  # the server's http.request
        raise_after = await receive()  # then the disconnect
        assert raise_after["type"] == "http.disconnect"
        raise asyncio.CancelledError

    messages: Iterator[dict[str, Any]] = iter(
        [
            {"type": "http.request", "body": b"", "more_body": False},
            {"type": "http.disconnect"},
        ]
    )

    async def receive() -> dict[str, Any]:
        return next(messages)

    async def send(message: Any) -> None:
        return None

    scope: dict[str, Any] = {"type": "http", "path": "/x", "method": "GET"}
    with pytest.raises(asyncio.CancelledError):
        asyncio.run(MetricsMiddleware(sees_disconnect, client=client)(scope, receive, send))
    assert "status:499,status_class:4xx" in "".join(datagrams(agent, client))


def test_cancelled_with_no_disconnect_is_the_servers_doing_so_500(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def cancelled(scope: Any, receive: Any, send: Any) -> None:
        raise asyncio.CancelledError  # a shutdown or a timeout scope, not a closed tab

    with pytest.raises(asyncio.CancelledError):
        run_request(cancelled, client)
    line = count_line(agent, client)
    assert "status:500,status_class:5xx" in line
    assert "status:499" not in line


def test_cancelled_after_response_started_keeps_the_sent_status(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def cancelled_mid_body(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        raise asyncio.CancelledError

    with pytest.raises(asyncio.CancelledError):
        run_request(cancelled_mid_body, client)
    assert "status:200" in "".join(datagrams(agent, client))


def test_app_that_never_answers_is_500(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()

    async def silent(scope: Any, receive: Any, send: Any) -> None:
        return None

    run_request(silent, client)
    assert "status:500" in "".join(datagrams(agent, client))


def test_only_the_first_response_start_counts(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()

    async def twice(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        await send({"type": "http.response.start", "status": 503, "headers": []})

    run_request(twice, client)
    assert "status:200" in "".join(datagrams(agent, client))


def test_websocket_and_lifespan_pass_through_unrecorded(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    seen: list[str] = []

    async def app(scope: Any, receive: Any, send: Any) -> None:
        seen.append(scope["type"])

    for kind in ("websocket", "lifespan"):
        run_request(app, client, scope_type=kind)
    client.flush()
    assert seen == ["websocket", "lifespan"]
    assert agent.recv_or_none(0.2) is None


def test_excluded_paths_are_not_recorded_but_are_served(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    sent = run_request(responder(200, "/health"), client, path="/health", exclude_paths=["/health"])
    assert sent[0]["status"] == 200
    client.flush()
    assert agent.recv_or_none(0.2) is None


def test_disabled_client_is_a_pure_passthrough() -> None:
    client = StatsdClient()
    client.configure(Config())
    sent = run_request(responder(200, "/r"), client)
    assert [m["type"] for m in sent] == ["http.response.start", "http.response.body"]


def test_a_recording_failure_never_reaches_the_request(
    make_client: ClientFactory,
) -> None:
    client = make_client()

    def broken(*args: Any, **kwargs: Any) -> None:
        raise RuntimeError("statsd exploded")

    client.increment = broken  # type: ignore[method-assign]
    sent = run_request(responder(200, "/r"), client)
    assert sent[0]["status"] == 200


def test_route_object_without_a_string_path_is_unmatched(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        scope["route"] = SimpleNamespace(path=None)
        await send({"type": "http.response.start", "status": 200, "headers": []})

    run_request(app, client)
    assert "route:unmatched" in "".join(datagrams(agent, client))


def test_custom_metric_names(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()
    run_request(responder(200, "/r"), client, count_name="web.hits", duration_name="web.ms")
    lines = datagrams(agent, client)
    assert any(line.startswith("web.hits:1|c") for line in lines)
    assert any(line.startswith("web.ms:") for line in lines)


@pytest.mark.parametrize(
    ("method", "expected"),
    [
        ("GET", "GET"),
        ("DELETE", "DELETE"),
        ("PROPFIND", "OTHER"),
        ("X1", "OTHER"),
        ("get", "OTHER"),
    ],
)
def test_method_tag_is_limited_to_the_standard_verbs(
    make_client: ClientFactory, agent: FakeAgent, method: str, expected: str
) -> None:
    client = make_client()
    run_request(responder(200, "/r"), client, method=method)
    assert f"method:{expected}," in "".join(datagrams(agent, client))


def test_a_missing_method_is_other_not_a_crash(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})

    async def receive() -> dict[str, Any]:
        return {"type": "http.request"}

    async def send(message: Any) -> None:
        return None

    middleware = MetricsMiddleware(app, client=client)
    asyncio.run(middleware({"type": "http", "path": "/x"}, receive, send))
    assert "method:OTHER," in "".join(datagrams(agent, client))


@pytest.mark.parametrize("bad", [None, "200", 0, 99, 600, 12345, True])
def test_a_bad_status_from_the_app_never_breaks_the_response_and_records_500(
    make_client: ClientFactory, agent: FakeAgent, bad: Any
) -> None:
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": bad, "headers": []})
        await send({"type": "http.response.body", "body": b"ok"})

    sent = run_request(app, client)
    assert [m["type"] for m in sent] == ["http.response.start", "http.response.body"]
    line = count_line(agent, client)
    assert "status:500,status_class:5xx" in line
    assert "status:0" not in line


def test_a_handler_error_after_the_client_disconnected_is_499(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    """Starlette raises ClientDisconnect, and a write to a closed socket raises OSError: a
    closed tab, not a server fault, and not a CancelledError either."""
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await receive()
        raise ConnectionResetError("client went away")

    messages: Iterator[dict[str, Any]] = iter([{"type": "http.disconnect"}])

    async def receive() -> dict[str, Any]:
        return next(messages)

    async def send(message: Any) -> None:
        return None

    scope: dict[str, Any] = {"type": "http", "path": "/x", "method": "GET"}
    with pytest.raises(ConnectionResetError):
        asyncio.run(MetricsMiddleware(app, client=client)(scope, receive, send))
    assert "status:499,status_class:4xx" in count_line(agent, client)


def test_a_handler_bug_after_a_disconnect_is_still_a_500(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    """The disconnect only decides for failures that look like a closed connection; a KeyError
    that follows one is a bug and must reach the 5xx metrics."""
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await receive()
        raise KeyError("a real bug")

    messages: Iterator[dict[str, Any]] = iter([{"type": "http.disconnect"}])

    async def receive() -> dict[str, Any]:
        return next(messages)

    async def send(message: Any) -> None:
        return None

    scope: dict[str, Any] = {"type": "http", "path": "/x", "method": "GET"}
    with pytest.raises(KeyError):
        asyncio.run(MetricsMiddleware(app, client=client)(scope, receive, send))
    line = count_line(agent, client)
    assert "status:500,status_class:5xx" in line
    assert "status:499" not in line


@pytest.mark.parametrize("name", ["ClientDisconnect", "Cancelled"])
def test_framework_disconnect_errors_are_recognised_by_name(
    make_client: ClientFactory, agent: FakeAgent, name: str
) -> None:
    """Starlette's ClientDisconnect and trio's Cancelled are not importable here (no
    dependencies), so they are matched by class name."""
    client = make_client()
    error = type(name, (Exception,), {})

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await receive()
        raise error()

    messages: Iterator[dict[str, Any]] = iter([{"type": "http.disconnect"}])

    async def receive() -> dict[str, Any]:
        return next(messages)

    async def send(message: Any) -> None:
        return None

    scope: dict[str, Any] = {"type": "http", "path": "/x", "method": "GET"}
    with pytest.raises(error):
        asyncio.run(MetricsMiddleware(app, client=client)(scope, receive, send))
    assert "status:499,status_class:4xx" in count_line(agent, client)


def test_a_failed_write_of_the_response_start_is_not_recorded_with_the_apps_status(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    """The client closed the socket before the start message was written: nothing reached it,
    so the app's 200 must not be what gets counted."""
    client = make_client()

    async def app(scope: Any, receive: Any, send: Any) -> None:
        await receive()
        await send({"type": "http.response.start", "status": 200, "headers": []})

    messages: Iterator[dict[str, Any]] = iter([{"type": "http.disconnect"}])

    async def receive() -> dict[str, Any]:
        return next(messages)

    async def send(message: Any) -> None:
        raise BrokenPipeError("socket closed")

    scope: dict[str, Any] = {"type": "http", "path": "/x", "method": "GET"}
    with pytest.raises(BrokenPipeError):
        asyncio.run(MetricsMiddleware(app, client=client)(scope, receive, send))
    line = count_line(agent, client)
    assert "status:499,status_class:4xx" in line
    assert "status:200" not in line


def test_a_bare_string_for_exclude_paths_is_one_path_not_its_characters(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    run_request(responder(200, "/healthz"), client, path="/healthz", exclude_paths="/healthz")
    client.flush()
    assert agent.recv_or_none(0.2) is None  # excluded
    run_request(responder(200, "/"), client, path="/", exclude_paths="/healthz")
    assert "route:/," in count_line(agent, client)  # "/" is NOT swallowed as a character


def test_a_streamed_response_is_one_request_and_its_duration_includes_the_stream(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    """Several body chunks (SSE, a download) are one request with the status it started with,
    every chunk reaches the client untouched, and the duration runs until the stream ends, so a
    long stream is a long request (documented in docs/sdk/python.md)."""
    client = make_client()
    chunks = [b"data: 1\n\n", b"data: 2\n\n", b"data: 3\n\n"]

    async def streamer(scope: Any, receive: Any, send: Any) -> None:
        await send({"type": "http.response.start", "status": 200, "headers": []})
        for chunk in chunks:
            await send({"type": "http.response.body", "body": chunk, "more_body": True})
            await asyncio.sleep(0.06)
        await send({"type": "http.response.body", "body": b"", "more_body": False})

    sent = run_request(streamer, client, path="/events")
    assert [m.get("body") for m in sent[1:-1]] == chunks  # nothing lost or altered
    lines = datagrams(agent, client)
    assert sum(line.startswith("http.request.count:1|c") for line in lines) == 1
    assert any("status:200,status_class:2xx" in line for line in lines)
    duration = next(line for line in lines if line.startswith("http.request.duration:"))
    # Three sleeps of 60 ms: a lower bound only, so a slow machine cannot flake it.
    assert float(duration.split(":", 1)[1].split("|", 1)[0]) >= 150.0
