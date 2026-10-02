"""The middleware against a real Starlette app, where the route comes from framework internals.

A hand-built scope proves the middleware reads ``scope["route"]``; only a real app proves
Starlette puts the matched route there, and that it is what the docs say it is. If a
Starlette release stops setting it, every route would silently become ``unmatched``.
"""

from __future__ import annotations

import asyncio
import contextlib
from typing import Any

import pytest

pytest.importorskip("starlette")

from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import PlainTextResponse
from starlette.routing import Mount, Route

from ozy.integrations.asgi import MetricsMiddleware

from .conftest import ClientFactory, FakeAgent


async def item(request: Request) -> PlainTextResponse:
    return PlainTextResponse(request.path_params["item_id"])


async def broken(request: Request) -> PlainTextResponse:
    raise RuntimeError("handler failed")


def call(app: Any, path: str, method: str = "GET") -> list[dict[str, Any]]:
    sent: list[dict[str, Any]] = []

    async def receive() -> dict[str, Any]:
        return {"type": "http.request", "body": b"", "more_body": False}

    async def send(message: Any) -> None:
        sent.append(dict(message))

    scope = {
        "type": "http",
        "asgi": {"version": "3.0"},
        "http_version": "1.1",
        "method": method,
        "path": path,
        "raw_path": path.encode(),
        "root_path": "",
        "scheme": "http",
        "query_string": b"",
        "headers": [],
        "client": ("test", 1),
        "server": ("test", 80),
    }
    # ServerErrorMiddleware re-raises after sending its 500, as a server expects.
    with contextlib.suppress(RuntimeError):
        asyncio.run(app(scope, receive, send))
    return sent


def tags_of(agent: FakeAgent, client: Any) -> str:
    client.flush()
    return "".join(agent.recv().splitlines())


def build(client: Any, **kwargs: Any) -> Starlette:
    app = Starlette(
        routes=[
            Route("/items/{item_id}", item),
            Route("/broken", broken),
            Mount("/m", app=Starlette(routes=[Route("/y/{item_id}", item)])),
        ]
    )
    app.add_middleware(MetricsMiddleware, client=client, **kwargs)
    return app


def test_a_matched_route_is_tagged_with_its_pattern_not_the_raw_path(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    call(build(client), "/items/abc123")
    seen = tags_of(agent, client)
    assert "route:/items/{item_id}," in seen
    assert "abc123" not in seen
    assert "status:200," in seen


def test_an_unknown_path_is_unmatched_with_its_real_status(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    call(build(client), "/wp-login.php")
    seen = tags_of(agent, client)
    assert "route:unmatched," in seen
    assert "status:404," in seen


def test_a_handler_that_raises_is_recorded_as_500(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    call(build(client), "/broken")
    assert "route:/broken,method:GET,status:500," in tags_of(agent, client)


def test_a_mounted_sub_app_gets_a_bounded_tag_whatever_the_starlette_version(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    """Whether a mount's route is reported relative or not at all differs between Starlette
    releases, so this pins only what the docs promise: a pattern or ``unmatched``, not the path.
    """
    client = make_client()
    call(build(client), "/m/y/42")
    seen = tags_of(agent, client)
    assert "route:/y/{item_id}," in seen or "route:unmatched," in seen
    assert "42" not in seen


def test_exclude_paths_matches_the_exact_raw_path(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    app = build(client, exclude_paths=["/items/skipped"])
    call(app, "/items/skipped")
    client.flush()
    assert agent.recv_or_none(0.2) is None
    call(app, "/items/skipped/")  # the trailing slash is a different path: documented
    assert "status:" in tags_of(agent, client)
