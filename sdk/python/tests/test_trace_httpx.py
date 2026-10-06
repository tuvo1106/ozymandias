"""httpx integration with ``MockTransport``: the span, the injected headers, what never leaks."""

from __future__ import annotations

import asyncio
from typing import Any

import httpx
import pytest

import ozy
from ozy.integrations.httpx import INTEGRATION

from .conftest import FakeTraceAgent

SECRET = "sentinel-secret-5521"


@pytest.fixture(autouse=True)
def _reset_inject_hosts() -> Any:
    yield
    INTEGRATION.inject_hosts = ()


def make_handler(seen: list[httpx.Request], status: int = 200) -> Any:
    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(request)
        return httpx.Response(status, text="ok")

    return handler


def client_spans(agent: FakeTraceAgent) -> list[dict[str, Any]]:
    ozy.tracer.flush()
    return [s for s in agent.spans() if s["name"] == "http.client"]


def test_a_sync_request_is_a_span_and_carries_the_trace_headers(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    INTEGRATION.inject_hosts = {"payments.internal"}
    seen: list[httpx.Request] = []
    with httpx.Client(transport=httpx.MockTransport(make_handler(seen))) as client:
        with ozy.tracer.trace("request") as root:
            response = client.get("https://payments.internal/charge?token=" + SECRET)
    assert response.status_code == 200
    (span,) = client_spans(traced)
    assert span["type"] == "http"
    assert span["resource"] == "GET payments.internal"
    assert span["parent_id"] == root.span_id
    assert span["meta"]["http.method"] == "GET"
    assert span["meta"]["http.url"] == "https://payments.internal/charge"
    assert span["meta"]["http.status_code"] == "200"
    assert span["meta"]["span.kind"] == "client"
    assert span["error"] == 0
    # The downstream service continues the trace *from the client span*, not from the root.
    (request,) = seen
    ctx = ozy.tracer.extract(dict(request.headers))
    assert ctx is not None
    assert (ctx.trace_id, ctx.span_id) == (root.trace_id, span["span_id"])
    assert ctx.sampling_priority == 1
    assert all(SECRET.encode() not in b for b in traced.raw)  # no query string in any span


def test_an_async_request_is_traced_the_same_way(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    INTEGRATION.inject_hosts = {"api.example.com"}
    seen: list[httpx.Request] = []

    async def run() -> None:
        async with httpx.AsyncClient(transport=httpx.MockTransport(make_handler(seen))) as client:
            async with ozy.tracer.trace("request"):
                await client.post("https://api.example.com/v1/things", json={"a": 1})

    asyncio.run(run())
    (span,) = client_spans(traced)
    assert span["resource"] == "POST api.example.com"
    assert "x-ozy-trace-id" in seen[0].headers


def test_credentials_and_port_handling_in_the_url_tag(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    with httpx.Client(transport=httpx.MockTransport(make_handler([]))) as client:
        with ozy.tracer.trace("request"):
            client.get(f"http://user:{SECRET}@localhost:8080/a/b?x=1#frag")
    (span,) = client_spans(traced)
    assert span["meta"]["http.url"] == "http://localhost:8080/a/b"
    assert span["resource"] == "GET localhost"
    assert all(SECRET.encode() not in b for b in traced.raw)


def test_a_5xx_answer_is_an_error_and_a_4xx_is_not(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    for status in (404, 502):
        with httpx.Client(transport=httpx.MockTransport(make_handler([], status))) as client:
            with ozy.tracer.trace(f"r{status}"):
                client.get("https://h.example/x")
    spans = {s["meta"]["http.status_code"]: s for s in client_spans(traced)}
    assert spans["404"]["error"] == 0
    assert spans["502"]["error"] == 1


def test_a_transport_error_marks_the_span_and_reraises_unchanged(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused", request=request)

    with httpx.Client(transport=httpx.MockTransport(handler)) as client:
        with ozy.tracer.trace("request"), pytest.raises(httpx.ConnectError, match="refused"):
            client.get("https://down.example/")
    (span,) = client_spans(traced)
    assert span["error"] == 1
    assert span["meta"]["error.type"] == "httpx.ConnectError"
    assert "http.status_code" not in span["meta"]


def test_a_request_outside_a_trace_gets_no_span_and_no_headers(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    seen: list[httpx.Request] = []
    with httpx.Client(transport=httpx.MockTransport(make_handler(seen))) as client:
        client.get("https://h.example/")
    assert client_spans(traced) == []
    assert "x-ozy-trace-id" not in seen[0].headers


def test_redirects_are_one_span(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/start":
            return httpx.Response(302, headers={"location": "/end"})
        return httpx.Response(200)

    with httpx.Client(transport=httpx.MockTransport(handler), follow_redirects=True) as client:
        with ozy.tracer.trace("request"):
            assert client.get("https://h.example/start").status_code == 200
    assert len(client_spans(traced)) == 1


def test_inject_hosts_restricts_the_headers_but_not_the_span(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    INTEGRATION.inject_hosts = {"payments.internal"}
    try:
        seen: list[httpx.Request] = []
        with httpx.Client(transport=httpx.MockTransport(make_handler(seen))) as client:
            with ozy.tracer.trace("request"):
                client.get("https://payments.internal/a")
                client.get("https://third-party.example/b")
    finally:
        INTEGRATION.inject_hosts = ()
    assert [("x-ozy-trace-id" in r.headers) for r in seen] == [True, False]
    assert len(client_spans(traced)) == 2


def test_patch_is_idempotent_and_unpatch_restores(traced: FakeTraceAgent) -> None:
    original = httpx.Client.send
    INTEGRATION.patch()
    INTEGRATION.patch()
    assert httpx.Client.send.__wrapped__ is original  # type: ignore[attr-defined]
    INTEGRATION.unpatch()
    assert httpx.Client.send is original
    with httpx.Client(transport=httpx.MockTransport(make_handler([]))) as client:
        with ozy.tracer.trace("request"):
            client.get("https://h.example/")
    assert client_spans(traced) == []


def test_unavailable_library_is_a_noop(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real = builtins.__import__

    def fake(name: str, *a: Any, **k: Any) -> Any:
        if name == "httpx":
            raise ImportError(name)
        return real(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", fake)
    assert INTEGRATION.is_available() is False
    INTEGRATION.patch()


def test_by_default_no_host_receives_the_propagation_headers(traced: FakeTraceAgent) -> None:
    # A trace id and a sampling decision are not for third parties; the Node SDK agrees.
    INTEGRATION.patch()
    seen: list[httpx.Request] = []
    with httpx.Client(transport=httpx.MockTransport(make_handler(seen))) as client:
        with ozy.tracer.trace("request"):
            client.get("https://third-party.example/b")
    assert not any(k.startswith("x-ozy-") for k in seen[0].headers)
    assert len(client_spans(traced)) == 1
