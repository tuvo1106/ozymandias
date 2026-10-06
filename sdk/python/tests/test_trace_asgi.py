"""``TraceMiddleware`` against a real FastAPI app (route patterns come from its internals)."""

from __future__ import annotations

import asyncio
from typing import Any

import pytest
from fastapi import APIRouter, FastAPI, HTTPException, WebSocket
from fastapi.responses import StreamingResponse
from fastapi.testclient import TestClient

import ozy
from ozy import StatsdClient, Tracer
from ozy.integrations.asgi import INTEGRATION, MetricsMiddleware, TraceMiddleware

from .conftest import ClientFactory, FakeAgent, FakeTraceAgent

SECRET = "sentinel-secret-9137"


def build(**mw: Any) -> FastAPI:
    app = FastAPI()
    router = APIRouter(prefix="/api/v1")

    @router.get("/items/{item_id}")
    async def item(item_id: int) -> dict[str, int]:
        with ozy.tracer.trace("handler.work") as span:
            span.set_tag("item", item_id)
        return {"id": item_id}

    @router.get("/sync/{name}")
    def sync_item(name: str) -> dict[str, str]:  # runs in a worker thread
        with ozy.tracer.trace("sync.work"):
            pass
        return {"name": name}

    @router.post("/boom")
    async def boom() -> None:
        raise RuntimeError("handler exploded")

    @router.get("/teapot")
    async def teapot() -> None:
        raise HTTPException(status_code=418)

    @router.get("/unavailable")
    async def unavailable() -> None:
        raise HTTPException(status_code=503, detail="try later")

    @router.get("/stream")
    async def stream() -> StreamingResponse:
        async def gen() -> Any:
            for i in range(3):
                await asyncio.sleep(0)
                yield f"chunk{i}\n".encode()

        return StreamingResponse(gen())

    @app.get("/health")
    async def health() -> dict[str, bool]:
        return {"ok": True}

    @app.websocket("/ws")
    async def ws(socket: WebSocket) -> None:
        await socket.accept()
        await socket.send_text("hi")
        await socket.close()

    sub = FastAPI()

    @sub.get("/y/{item_id}")
    async def sub_item(item_id: int) -> dict[str, int]:
        return {"id": item_id}

    app.include_router(router)
    app.mount("/m", sub)
    app.add_middleware(TraceMiddleware, **mw)
    return app


def spans_of(agent: FakeTraceAgent) -> list[dict[str, Any]]:
    ozy.tracer.flush()
    return agent.spans()


def request_span(agent: FakeTraceAgent) -> dict[str, Any]:
    (span,) = [s for s in spans_of(agent) if s["name"] == "http.request"]
    return span


def test_a_request_becomes_one_web_span_with_the_route_pattern(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    assert client.get("/api/v1/items/42?token=" + SECRET).status_code == 200
    span = request_span(traced)
    assert span["type"] == "web"
    assert span["resource"] == "GET /api/v1/items/{item_id}"  # the pattern, not /items/42
    assert span["parent_id"] is None
    assert span["error"] == 0
    assert span["meta"]["http.method"] == "GET"
    assert span["meta"]["http.route"] == "/api/v1/items/{item_id}"
    assert span["meta"]["http.status_code"] == "200"
    assert span["meta"]["http.url"] == "/api/v1/items/42"
    assert span["meta"]["span.kind"] == "server"
    assert span["metrics"]["_top_level"] == 1
    assert SECRET not in str(traced.raw)  # the query string never reaches a span


def test_spans_made_while_handling_the_request_are_its_children(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    client.get("/api/v1/items/1")
    client.get("/api/v1/sync/x")  # a sync endpoint runs on a thread: context must follow
    spans = spans_of(traced)
    root_a = next(s for s in spans if s["resource"] == "GET /api/v1/items/{item_id}")
    root_b = next(s for s in spans if s["resource"] == "GET /api/v1/sync/{name}")
    work = next(s for s in spans if s["name"] == "handler.work")
    sync_work = next(s for s in spans if s["name"] == "sync.work")
    assert (work["trace_id"], work["parent_id"]) == (root_a["trace_id"], root_a["span_id"])
    assert (sync_work["trace_id"], sync_work["parent_id"]) == (
        root_b["trace_id"],
        root_b["span_id"],
    )
    assert root_a["trace_id"] != root_b["trace_id"]
    assert "_top_level" not in work["metrics"]


def test_an_unmatched_request_falls_back_to_the_normalized_path(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    assert client.get("/nope/123/abc").status_code == 404
    span = request_span(traced)
    assert span["resource"] == "GET /nope/:id/abc"
    assert "http.route" not in span["meta"]
    assert span["meta"]["http.status_code"] == "404"
    assert span["error"] == 0  # a 404 is the client's mistake


def test_included_router_prefix_is_in_the_pattern_and_mounted_apps_are_observed(
    traced: FakeTraceAgent,
) -> None:
    client = TestClient(build())
    client.get("/api/v1/items/5")
    client.get("/m/y/5")
    spans = [s for s in spans_of(traced) if s["name"] == "http.request"]
    by_url = {s["meta"]["http.url"]: s["resource"] for s in spans}
    assert by_url["/api/v1/items/5"] == "GET /api/v1/items/{item_id}"
    # What a mount reports depends on the Starlette version (see the asgi module docstring):
    # it is either the mount-relative pattern or the normalized path, never a raw id.
    assert by_url["/m/y/5"] in {"GET /y/{item_id}", "GET /m/y/:id"}


def test_an_unhandled_exception_marks_the_span_and_the_500(traced: FakeTraceAgent) -> None:
    client = TestClient(build(), raise_server_exceptions=False)
    assert client.post("/api/v1/boom").status_code == 500
    span = request_span(traced)
    assert span["error"] == 1
    assert span["meta"]["http.status_code"] == "500"
    assert span["meta"]["error.type"] == "RuntimeError"
    assert span["meta"]["error.message"] == "handler exploded"
    assert span["resource"] == "POST /api/v1/boom"


def test_the_exception_still_reaches_the_server_unchanged(traced: FakeTraceAgent) -> None:
    client = TestClient(build())  # raise_server_exceptions=True: the test client re-raises it
    with pytest.raises(RuntimeError, match="handler exploded"):
        client.post("/api/v1/boom")
    assert request_span(traced)["error"] == 1


def test_a_5xx_response_without_an_exception_is_an_error(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    assert client.get("/api/v1/unavailable").status_code == 503
    span = request_span(traced)
    assert span["error"] == 1
    assert "error.type" not in span["meta"]  # nothing was raised: only the status says so


def test_a_4xx_is_not_an_error(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    assert client.get("/api/v1/teapot").status_code == 418
    assert request_span(traced)["error"] == 0


def test_a_streaming_response_is_one_span_covering_the_stream(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    assert client.get("/api/v1/stream").text == "chunk0\nchunk1\nchunk2\n"
    span = request_span(traced)
    assert span["resource"] == "GET /api/v1/stream"
    assert span["meta"]["http.status_code"] == "200"


def test_websockets_and_lifespan_pass_through(traced: FakeTraceAgent) -> None:
    with TestClient(build()) as client, client.websocket_connect("/ws") as ws:  # lifespan runs too
        assert ws.receive_text() == "hi"
    assert spans_of(traced) == []


def test_upstream_context_is_continued_from_the_headers(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    client.get(
        "/api/v1/items/1",
        headers={
            "x-ozy-trace-id": "ABCDEF0123456789ABCDEF0123456789",  # uppercase is accepted
            "x-ozy-parent-id": "0123456789abcdef",
            "x-ozy-sampling-priority": "2",
        },
    )
    span = request_span(traced)
    assert span["trace_id"] == "abcdef0123456789abcdef0123456789"
    assert span["parent_id"] == "0123456789abcdef"
    assert span["metrics"]["_sampling_priority"] == 2
    assert span["metrics"]["_top_level"] == 1  # still the entry span of this service


@pytest.mark.parametrize(
    "headers",
    [
        {"x-ozy-trace-id": "short", "x-ozy-parent-id": "0123456789abcdef"},
        {"x-ozy-trace-id": "0" * 32, "x-ozy-parent-id": "0123456789abcdef"},
        {"x-ozy-trace-id": "a" * 32},
        {"x-ozy-trace-id": "a" * 32, "x-ozy-parent-id": "b" * 16, "x-ozy-sampling-priority": "9"},
        {"x-ozy-trace-id": "z" * 32, "x-ozy-parent-id": "b" * 16},
    ],
)
def test_malformed_headers_start_a_fresh_trace(
    traced: FakeTraceAgent, headers: dict[str, str]
) -> None:
    TestClient(build()).get("/api/v1/items/1", headers=headers)
    span = request_span(traced)
    assert span["parent_id"] is None
    assert span["trace_id"] != headers["x-ozy-trace-id"]


def test_a_rate_zero_trace_is_still_sent_as_priority_zero(
    trace_agent: FakeTraceAgent,
) -> None:
    ozy.tracer.configure(
        ozy.Config(agent_host="127.0.0.1", trace_port=trace_agent.port, trace_sample_rate=0.0)
    )
    TestClient(build()).get("/api/v1/items/1")
    assert request_span(trace_agent)["metrics"]["_sampling_priority"] == 0


def test_hostile_methods_and_paths_stay_low_cardinality(traced: FakeTraceAgent) -> None:
    client = TestClient(build())
    for method in ["BREW", "PROPFIND", "WHATEVER-X"]:
        client.request(method, "/api/v1/items/1")
    for i in range(30):
        client.get(f"/scan/{i}/{i * 7919}/x")
    resources = {s["resource"] for s in spans_of(traced) if s["name"] == "http.request"}
    methods = {r.split(" ")[0] for r in resources}
    assert methods <= {"GET", "OTHER"}
    assert "GET /scan/:id/:id/x" in resources
    assert len(resources) <= 4


def test_excluded_paths_are_neither_traced_nor_counted(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    TestClient(build(exclude_paths=["/health"], client=client)).get("/health")
    client.flush()
    assert agent.recv_or_none(0.1) is None
    assert spans_of(traced) == []


def test_one_middleware_emits_the_span_and_the_request_metrics(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    TestClient(build(client=client), raise_server_exceptions=False).post("/api/v1/boom")
    client.flush()
    lines = agent.recv().splitlines()
    count = next(line for line in lines if line.startswith("http.request.count"))
    assert "route:/api/v1/boom" in count
    assert "method:post" in count.lower()
    assert "status:500" in count
    span = request_span(traced)
    assert span["meta"]["http.status_code"] == "500"  # the span and the metric agree
    assert span["meta"]["http.route"] == "/api/v1/boom"


def test_metrics_still_flow_when_tracing_is_off(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    assert not ozy.tracer.enabled
    assert TestClient(build(client=client)).get("/api/v1/items/3").status_code == 200
    client.flush()
    assert "http.request.count" in agent.recv()


def test_tracing_still_works_when_metrics_are_off(traced: FakeTraceAgent) -> None:
    assert not ozy.statsd.enabled
    TestClient(build()).get("/api/v1/items/3")
    assert request_span(traced)["resource"] == "GET /api/v1/items/{item_id}"


def test_nothing_is_recorded_and_nothing_breaks_when_everything_is_off() -> None:
    assert TestClient(build()).get("/api/v1/items/3").status_code == 200


def test_a_failure_inside_the_tracer_never_reaches_the_request(
    traced: FakeTraceAgent, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("tracer is broken")

    monkeypatch.setattr(Tracer, "start_span", boom)
    assert TestClient(build()).get("/health").status_code == 200


def test_a_failure_while_finishing_the_span_never_reaches_the_request(
    traced: FakeTraceAgent, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("tracer is broken")

    monkeypatch.setattr(ozy.Span, "set_tag", boom)
    assert TestClient(build()).get("/health").status_code == 200


def test_spans_of_one_request_form_one_chunk(traced: FakeTraceAgent) -> None:
    TestClient(build()).get("/api/v1/items/1")
    ozy.tracer.flush()
    (chunk,) = traced.chunks()
    assert {s["name"] for s in chunk} == {"http.request", "handler.work"}


# -- the integration (patch) -------------------------------------------------------


def test_patch_traces_apps_with_no_middleware_added(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    app = FastAPI()

    @app.get("/auto/{x}")
    async def auto(x: int) -> dict[str, int]:
        return {"x": x}

    assert TestClient(app).get("/auto/1").status_code == 200
    assert request_span(traced)["resource"] == "GET /auto/{x}"


def test_patch_sees_the_500_from_the_server_error_middleware(traced: FakeTraceAgent) -> None:
    INTEGRATION.patch()
    app = FastAPI()

    @app.get("/x")
    async def x() -> None:
        raise ValueError("nope")

    with pytest.raises(ValueError, match="nope"):
        TestClient(app).get("/x")
    span = request_span(traced)
    assert span["error"] == 1
    assert span["meta"]["http.status_code"] == "500"


def test_patch_leaves_alone_an_app_that_already_has_the_middleware(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    INTEGRATION.patch()
    client = make_client()
    app = FastAPI()
    app.add_middleware(MetricsMiddleware, client=client)

    @app.get("/once")
    async def once() -> str:
        return "ok"

    TestClient(app).get("/once")
    client.flush()
    counts = [line for line in agent.recv().splitlines() if line.startswith("http.request.count")]
    assert len(counts) == 1  # one request, one count: never doubled
    assert spans_of(traced) == []  # the user chose the metrics-only middleware


def test_patch_is_idempotent_and_unpatch_restores(traced: FakeTraceAgent) -> None:
    from starlette.applications import Starlette

    original = Starlette.build_middleware_stack
    INTEGRATION.patch()
    INTEGRATION.patch()
    wrapped = Starlette.build_middleware_stack
    assert wrapped is not original
    assert getattr(wrapped, "__ozy_wrapped__", False)
    assert wrapped.__wrapped__ is original  # type: ignore[attr-defined]  # wrapped once, not twice
    INTEGRATION.unpatch()
    assert Starlette.build_middleware_stack is original
    app = FastAPI()
    TestClient(app).get("/missing")
    assert spans_of(traced) == []


def test_integration_metadata() -> None:
    assert INTEGRATION.name == "asgi"
    assert INTEGRATION.is_available()


def test_a_private_tracer_and_client_can_be_injected(
    make_client: ClientFactory, trace_agent: FakeTraceAgent, make_tracer: Any
) -> None:
    private = make_tracer()
    client: StatsdClient = make_client()
    app = FastAPI()

    @app.get("/p")
    async def p() -> str:
        return "ok"

    app.add_middleware(TraceMiddleware, tracer=private, client=client)
    TestClient(app).get("/p")
    private.flush()
    assert [s["resource"] for s in trace_agent.spans()] == ["GET /p"]
