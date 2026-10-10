"""The WSGI middleware and the flask integration, against real Flask and against raw WSGI."""

from __future__ import annotations

import logging
import sys
import time
from collections.abc import Callable, Iterator
from typing import Any

import pytest
from flask import Blueprint, Flask, Response, abort, stream_with_context
from flask.testing import FlaskClient

import ozy
from ozy import StatsdClient, Tracer
from ozy.integrations.wsgi import (
    ERROR_KEY,
    INTEGRATION,
    ROUTE_KEY,
    MetricsMiddleware,
    TraceMiddleware,
)

from .conftest import ClientFactory, FakeAgent, FakeTraceAgent

SECRET = "sentinel-secret-4821"


class BufferedClient(FlaskClient):
    """Werkzeug's client closes an unbuffered body only when the test closes the response, and
    the request is recorded on close (PEP 3333), so requests here are buffered by default."""

    def open(self, *args: Any, **kwargs: Any) -> Any:
        kwargs.setdefault("buffered", True)
        return super().open(*args, **kwargs)


def build() -> Flask:
    app = Flask(__name__)
    app.test_client_class = BufferedClient
    api = Blueprint("api", __name__, url_prefix="/api/v1")

    @api.get("/items/<int:item_id>")
    def item(item_id: int) -> dict[str, int]:
        with ozy.tracer.trace("handler.work") as span:
            span.set_tag("item", item_id)
        return {"id": item_id}

    @api.post("/boom")
    def boom() -> str:
        raise RuntimeError("handler exploded")

    @api.get("/teapot")
    def teapot() -> str:
        abort(418)

    @api.get("/unavailable")
    def unavailable() -> tuple[str, int]:
        return "try later", 503

    @api.get("/stream")
    def stream() -> Response:
        def gen() -> Iterator[str]:
            for i in range(3):
                with ozy.tracer.trace("stream.chunk"):
                    time.sleep(0.01)
                yield f"chunk{i}\n"

        return Response(stream_with_context(gen()))

    @app.get("/health")
    def health() -> str:
        return "ok"

    app.register_blueprint(api)
    return app


def spans_of(agent: FakeTraceAgent) -> list[dict[str, Any]]:
    ozy.tracer.flush()
    return agent.spans()


def request_span(agent: FakeTraceAgent) -> dict[str, Any]:
    (span,) = [s for s in spans_of(agent) if s["name"] == "http.request"]
    return span


def metrics(agent: FakeAgent, client: StatsdClient) -> list[tuple[str, str, dict[str, str]]]:
    """Every metric line flushed, parsed into ``(name, type, tags)``: never searched as text,
    because the duration line holds a measured number."""
    client.flush()
    lines = agent.recv().splitlines()
    while (more := agent.recv_or_none(0.05)) is not None:  # a long flush spans datagrams
        lines += more.splitlines()
    parsed = []
    for line in lines:
        head, _, tag_part = line.partition("|#")
        name_value, _, kind = head.partition("|")
        tags = dict(t.split(":", 1) for t in tag_part.split(",")) if tag_part else {}
        parsed.append((name_value.split(":", 1)[0], kind.split("|")[0], tags))
    return parsed


def count_tags(agent: FakeAgent, client: StatsdClient) -> list[dict[str, str]]:
    return [tags for name, _, tags in metrics(agent, client) if name == "http.request.count"]


@pytest.fixture
def flask_patched() -> Iterator[None]:
    INTEGRATION.patch()
    yield
    INTEGRATION.unpatch()


# -- Flask, through the integration ------------------------------------------------------


def test_a_request_becomes_one_web_span_with_the_route_pattern(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    client = build().test_client()
    assert client.get("/api/v1/items/42?token=" + SECRET).status_code == 200
    span = request_span(traced)
    assert span["type"] == "web"
    assert span["resource"] == "GET /api/v1/items/<int:item_id>"  # blueprint prefix included
    assert span["parent_id"] is None
    assert span["error"] == 0
    assert span["meta"]["http.method"] == "GET"
    assert span["meta"]["http.route"] == "/api/v1/items/<int:item_id>"
    assert span["meta"]["http.status_code"] == "200"
    assert span["meta"]["http.url"] == "/api/v1/items/42"
    assert span["meta"]["span.kind"] == "server"
    assert span["metrics"]["_top_level"] == 1
    assert SECRET not in str(traced.raw)  # the query string never reaches a span


def test_spans_made_by_the_handler_are_children_and_one_chunk(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    client = build().test_client()
    client.get("/api/v1/items/1")
    client.get("/api/v1/items/2")
    ozy.tracer.flush()
    chunks = traced.chunks()
    assert len(chunks) == 2  # one trace per request: the first did not leak into the second
    for chunk in chunks:
        root = next(s for s in chunk if s["name"] == "http.request")
        work = next(s for s in chunk if s["name"] == "handler.work")
        assert (work["trace_id"], work["parent_id"]) == (root["trace_id"], root["span_id"])
    assert chunks[0][0]["trace_id"] != chunks[1][0]["trace_id"]
    assert ozy.tracer.current_span() is None  # nothing left active on this thread


def test_an_app_built_before_patch_is_traced_too(traced: FakeTraceAgent) -> None:
    app = build()
    INTEGRATION.patch()
    try:
        app.test_client().get("/api/v1/items/1")
    finally:
        INTEGRATION.unpatch()
    assert request_span(traced)["resource"] == "GET /api/v1/items/<int:item_id>"


def test_an_unmatched_request_falls_back_to_the_normalized_path(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    assert build().test_client().get("/nope/123/abc").status_code == 404
    span = request_span(traced)
    assert span["resource"] == "GET /nope/:id/abc"
    assert "http.route" not in span["meta"]
    assert span["meta"]["http.status_code"] == "404"
    assert span["error"] == 0


def test_an_error_flask_turns_into_a_500_page_still_names_the_exception(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    assert build().test_client().post("/api/v1/boom").status_code == 500
    span = request_span(traced)
    assert span["error"] == 1
    assert span["meta"]["http.status_code"] == "500"
    assert span["meta"]["error.type"] == "RuntimeError"
    assert span["meta"]["error.message"] == "handler exploded"
    assert span["resource"] == "POST /api/v1/boom"


def test_a_propagated_exception_reaches_the_caller_unchanged(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    app = build()
    app.testing = True  # PROPAGATE_EXCEPTIONS: Flask re-raises instead of answering 500
    with pytest.raises(RuntimeError, match="handler exploded"):
        app.test_client().post("/api/v1/boom")
    span = request_span(traced)
    assert span["error"] == 1
    assert span["meta"]["http.status_code"] == "500"
    assert span["meta"]["error.type"] == "RuntimeError"


def test_a_5xx_without_an_exception_is_an_error_with_no_type(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    assert build().test_client().get("/api/v1/unavailable").status_code == 503
    span = request_span(traced)
    assert span["error"] == 1
    assert "error.type" not in span["meta"]


def test_a_4xx_is_not_an_error(traced: FakeTraceAgent, flask_patched: None) -> None:
    assert build().test_client().get("/api/v1/teapot").status_code == 418
    assert request_span(traced)["error"] == 0


def test_a_streamed_response_is_timed_to_the_end_of_the_stream(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    client = build().test_client()
    with client.get("/api/v1/stream", buffered=False) as response:
        assert spans_of(traced) == []  # nothing is recorded until the body is closed
        assert response.get_data(as_text=True) == "chunk0\nchunk1\nchunk2\n"
    spans = spans_of(traced)
    root = next(s for s in spans if s["name"] == "http.request")
    chunks = [s for s in spans if s["name"] == "stream.chunk"]
    assert len(chunks) == 3
    assert all(c["parent_id"] == root["span_id"] for c in chunks)
    assert root["duration"] >= 30_000  # three 10 ms chunks, in microseconds


def test_upstream_context_is_continued_from_the_headers(
    traced: FakeTraceAgent, flask_patched: None
) -> None:
    build().test_client().get(
        "/api/v1/items/1",
        headers={
            "x-ozy-trace-id": "ABCDEF0123456789ABCDEF0123456789",
            "x-ozy-parent-id": "0123456789abcdef",
            "x-ozy-sampling-priority": "2",
        },
    )
    span = request_span(traced)
    assert span["trace_id"] == "abcdef0123456789abcdef0123456789"
    assert span["parent_id"] == "0123456789abcdef"
    assert span["metrics"]["_sampling_priority"] == 2
    assert span["metrics"]["_top_level"] == 1


@pytest.mark.parametrize(
    "headers",
    [
        {"x-ozy-trace-id": "short", "x-ozy-parent-id": "0123456789abcdef"},
        {"x-ozy-trace-id": "0" * 32, "x-ozy-parent-id": "0123456789abcdef"},
        {"x-ozy-trace-id": "a" * 32},
        {"x-ozy-trace-id": "a" * 32, "x-ozy-parent-id": "b" * 16, "x-ozy-sampling-priority": "9"},
    ],
)
def test_malformed_headers_start_a_fresh_trace(
    traced: FakeTraceAgent, flask_patched: None, headers: dict[str, str]
) -> None:
    build().test_client().get("/api/v1/items/1", headers=headers)
    span = request_span(traced)
    assert span["parent_id"] is None
    assert span["trace_id"] != headers["x-ozy-trace-id"]


def test_hostile_methods_paths_and_statuses_stay_low_cardinality(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    app = build()
    app.wsgi_app = TraceMiddleware(app.wsgi_app, client=client)  # type: ignore[method-assign]
    test = app.test_client()
    for method in ["BREW", "PROPFIND", "WHATEVER-X"]:
        test.open("/api/v1/items/1", method=method)
    for i in range(30):
        test.get(f"/scan/{i}/{i * 7919}/x")
    resources = {s["resource"] for s in spans_of(traced) if s["name"] == "http.request"}
    assert {r.split(" ")[0] for r in resources} <= {"GET", "OTHER"}
    assert len(resources) <= 3
    tags = count_tags(agent, client)
    assert len(tags) == 33
    assert {t["method"] for t in tags} <= {"GET", "OTHER"}
    assert {t["route"] for t in tags} == {"unmatched"}  # by hand: no flask adapter, no pattern


def test_one_middleware_emits_the_span_and_the_request_metrics(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent, flask_patched: None
) -> None:
    client = make_client()
    app = build()
    app.wsgi_app = MetricsMiddleware(app.wsgi_app, client=client)  # type: ignore[method-assign]
    app.test_client().post("/api/v1/boom")
    (tags,) = count_tags(agent, client)
    # The hand-added metrics middleware is outermost and claims the request; the patched
    # wsgi_app inside it passes through, so there is one count and no span.
    assert tags == {
        "route": "/api/v1/boom",
        "method": "POST",
        "status": "500",
        "status_class": "5xx",
    }
    assert spans_of(traced) == []


def test_the_patched_app_counts_once_with_both_metric_names(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    app = build()
    app.wsgi_app = TraceMiddleware(app.wsgi_app, client=client)  # type: ignore[method-assign]
    INTEGRATION.patch()
    try:
        app.test_client().get("/api/v1/items/7")
    finally:
        INTEGRATION.unpatch()
    parsed = metrics(agent, client)
    assert [(n, k) for n, k, _ in parsed] == [
        ("http.request.count", "c"),
        ("http.request.duration", "d"),
    ]
    assert parsed[0][2]["route"] == "/api/v1/items/<int:item_id>"
    span = request_span(traced)  # exactly one span: the outer middleware claimed the request
    assert span["meta"]["http.route"] == parsed[0][2]["route"]


def test_excluded_paths_are_neither_traced_nor_counted(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    app = build()
    app.wsgi_app = TraceMiddleware(  # type: ignore[method-assign]
        app.wsgi_app, client=client, exclude_paths="/health"
    )
    assert app.test_client().get("/health").status_code == 200
    client.flush()
    assert agent.recv_or_none(0.1) is None
    assert spans_of(traced) == []


def test_metrics_still_flow_when_tracing_is_off(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    app = build()
    app.wsgi_app = TraceMiddleware(app.wsgi_app, client=client)  # type: ignore[method-assign]
    assert app.test_client().get("/api/v1/items/3").status_code == 200
    assert count_tags(agent, client)[0]["status"] == "200"


def test_nothing_breaks_when_everything_is_off(flask_patched: None) -> None:
    assert build().test_client().get("/api/v1/items/3").status_code == 200


def test_a_failure_inside_the_tracer_never_reaches_the_request(
    traced: FakeTraceAgent,
    flask_patched: None,
    monkeypatch: pytest.MonkeyPatch,
    caplog: pytest.LogCaptureFixture,
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("tracer is broken")

    monkeypatch.setattr(Tracer, "start_span", boom)
    with caplog.at_level(logging.DEBUG, logger="ozy"):
        assert build().test_client().get("/health").status_code == 200
    assert "starting the request span failed" in caplog.text  # the failure was reached


def test_a_failure_while_finishing_the_span_never_reaches_the_request(
    traced: FakeTraceAgent,
    make_client: ClientFactory,
    agent: FakeAgent,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def boom(*a: Any, **k: Any) -> Any:
        raise RuntimeError("tracer is broken")

    # Broken after the span started (breaking ``Span.set_tag`` would break ``start_span``
    # first, which hands back a no-op span and never reaches the finishing code).
    monkeypatch.setattr(TraceMiddleware, "_finish_span", staticmethod(boom))
    client = make_client()
    app = build()
    app.wsgi_app = TraceMiddleware(app.wsgi_app, client=client)  # type: ignore[method-assign]
    assert app.test_client().get("/health").status_code == 200
    assert count_tags(agent, client)[0]["status"] == "200"  # the metrics are still recorded


def test_a_failure_building_the_middleware_falls_back_to_the_plain_app(
    traced: FakeTraceAgent, flask_patched: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(*a: Any, **k: Any) -> None:
        raise RuntimeError("cannot build")

    monkeypatch.setattr(TraceMiddleware, "__init__", boom)
    assert build().test_client().get("/health").status_code == 200
    assert spans_of(traced) == []


def test_failures_reading_the_route_or_the_error_never_reach_the_request(
    traced: FakeTraceAgent, flask_patched: None, monkeypatch: pytest.MonkeyPatch
) -> None:
    import flask

    class Broken:
        def __getattr__(self, name: str) -> Any:
            raise RuntimeError("no request")

    app = build()
    client = app.test_client()
    monkeypatch.setattr(flask, "request", Broken())
    assert client.post("/api/v1/boom").status_code == 500
    span = request_span(traced)
    assert span["resource"] == "POST /api/v1/boom"  # the normalized path, not the pattern
    assert "error.type" not in span["meta"]


def test_patch_is_idempotent_and_unpatch_restores(traced: FakeTraceAgent) -> None:
    original = Flask.wsgi_app
    INTEGRATION.patch()
    INTEGRATION.patch()
    wrapped = Flask.wsgi_app
    assert wrapped is not original
    assert wrapped.__wrapped__ is original  # type: ignore[attr-defined]  # wrapped once
    INTEGRATION.unpatch()
    assert Flask.wsgi_app is original
    assert "full_dispatch_request" not in vars(Flask) or not getattr(
        Flask.full_dispatch_request, "__ozy_wrapped__", False
    )
    build().test_client().get("/api/v1/items/1")
    # The handler's own span is still sent, as a root of its own: no request wraps it now.
    assert [s["name"] for s in spans_of(traced)] == ["handler.work"]


def test_integration_metadata(monkeypatch: pytest.MonkeyPatch) -> None:
    assert INTEGRATION.name == "flask"
    assert INTEGRATION.is_available()
    monkeypatch.setitem(sys.modules, "flask", None)
    assert not INTEGRATION.is_available()
    INTEGRATION.patch()  # unavailable: a no-op, not an error
    assert not getattr(Flask.wsgi_app, "__ozy_wrapped__", False)


def test_patch_all_and_init_know_flask(traced: FakeTraceAgent) -> None:
    from ozy.integrations import get_integration, patch

    assert get_integration("flask") is INTEGRATION
    assert patch(["flask"]) == ["flask"]
    build().test_client().get("/api/v1/items/9")
    assert request_span(traced)["resource"] == "GET /api/v1/items/<int:item_id>"


# -- raw WSGI: the parts of PEP 3333 Flask never exercises --------------------------------

App = Callable[[dict[str, Any], Callable[..., Any]], Any]


def run(
    middleware: Any, *, path: str = "/x", method: str = "GET", **environ: Any
) -> tuple[list[str], bytes]:
    """Serve one request the way a WSGI server does; return the statuses sent and the body."""
    statuses: list[str] = []
    written: list[bytes] = []

    def start_response(status: str, headers: Any, exc_info: Any = None) -> Callable[..., None]:
        if exc_info is not None and written:
            raise exc_info[1].with_traceback(exc_info[2])
        statuses.append(status)
        return written.append

    env = {"REQUEST_METHOD": method, "PATH_INFO": path, "SCRIPT_NAME": "", **environ}
    body = middleware(env, start_response)
    try:
        for chunk in body:
            written.append(chunk)
    finally:
        close = getattr(body, "close", None)
        if close is not None:
            close()
    return statuses, b"".join(written)


def routed(app: App, route: str) -> App:
    def inner(environ: dict[str, Any], start_response: Callable[..., Any]) -> Any:
        environ[ROUTE_KEY] = route
        return app(environ, start_response)

    return inner


def test_a_failure_before_the_body_is_a_500_whatever_status_was_set(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> Iterator[bytes]:
        start_response("200 OK", [])
        raise ValueError("before any body")

    with pytest.raises(ValueError, match="before any body"):
        run(MetricsMiddleware(app, client=client))
    assert count_tags(agent, client)[0]["status"] == "500"


def test_a_failure_mid_body_keeps_the_status_sent_and_marks_the_span(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> Iterator[bytes]:
        start_response("200 OK", [])
        yield b"first"
        raise ValueError("mid body")

    with pytest.raises(ValueError, match="mid body"):
        run(TraceMiddleware(routed(app, "/r"), client=client))
    (tags,) = count_tags(agent, client)  # the failure and the close after it: one request
    assert tags["status"] == "200"
    span = request_span(traced)
    assert span["meta"]["http.status_code"] == "200"
    assert span["error"] == 1
    assert span["meta"]["error.type"] == "ValueError"
    assert ozy.tracer.current_span() is None


def test_a_status_replaced_with_exc_info_before_the_body_is_the_one_recorded(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        try:
            raise KeyError("x")
        except KeyError:
            start_response("503 Service Unavailable", [], sys.exc_info())
        return [b"sorry"]

    statuses, body = run(MetricsMiddleware(app, client=client))
    assert statuses == ["200 OK", "503 Service Unavailable"]
    assert body == b"sorry"
    assert count_tags(agent, client)[0]["status"] == "503"


def test_a_late_exc_info_call_changes_nothing_recorded(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> Iterator[bytes]:
        start_response("201 Created", [])
        yield b"sent"
        try:
            raise KeyError("late")
        except KeyError:
            start_response("500 Internal Server Error", [], sys.exc_info())  # re-raises

    with pytest.raises(KeyError):
        run(MetricsMiddleware(app, client=client))
    assert count_tags(agent, client)[0]["status"] == "201"


def test_the_write_callable_counts_as_answering(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> Iterator[bytes]:
        write = start_response("202 Accepted", [])
        write(b"")  # an empty write sends nothing
        write(b"legacy")
        yield b""
        raise ValueError("after write")

    with pytest.raises(ValueError, match="after write"):
        run(MetricsMiddleware(app, client=client))
    assert count_tags(agent, client)[0]["status"] == "202"


def test_an_empty_write_does_not_count_as_answering(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> Iterator[bytes]:
        start_response("202 Accepted", [])(b"")
        yield b""  # an empty chunk sends nothing either
        raise ValueError("nothing sent")

    with pytest.raises(ValueError, match="nothing sent"):
        run(MetricsMiddleware(app, client=client))
    assert count_tags(agent, client)[0]["status"] == "500"


@pytest.mark.parametrize(
    "status",
    ["OK", "2000 Huge", "abc Letters", "99 Low", "600 High", "²⁰⁰ Superscript", "", "200Glued"],
)
def test_a_bad_status_line_is_recorded_as_500_never_a_new_series(
    make_client: ClientFactory, agent: FakeAgent, status: str
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response(status, [])
        return [b"x"]

    statuses, _ = run(MetricsMiddleware(app, client=client))
    assert statuses == [status]  # passed to the server untouched
    assert count_tags(agent, client)[0]["status"] == "500"


def test_a_bare_status_code_with_no_reason_is_accepted(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("204", [])
        return []

    run(MetricsMiddleware(app, client=client))
    assert count_tags(agent, client)[0]["status"] == "204"


def test_an_app_that_never_starts_a_response_is_500(
    make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    run(MetricsMiddleware(lambda environ, start_response: [], client=client))
    assert count_tags(agent, client)[0]["status"] == "500"


def test_the_request_is_recorded_once_when_the_body_is_closed(
    traced: FakeTraceAgent, make_client: ClientFactory, agent: FakeAgent
) -> None:
    client = make_client()
    closed: list[bool] = []

    class Body:
        def __iter__(self) -> Iterator[bytes]:
            yield b"a"

        def close(self) -> None:
            closed.append(True)

    def app(environ: Any, start_response: Any) -> Body:
        start_response("200 OK", [])
        return Body()

    middleware = TraceMiddleware(app, client=client)
    body = middleware({"REQUEST_METHOD": "GET", "PATH_INFO": "/x"}, lambda *a: lambda data: None)
    assert list(body) == [b"a"]
    assert spans_of(traced) == []  # exhausted but not closed: still in flight
    body.close()  # type: ignore[attr-defined]
    body.close()  # type: ignore[attr-defined]  # a second close records nothing more
    assert closed == [True, True]
    assert len([s for s in spans_of(traced) if s["name"] == "http.request"]) == 1
    assert len(count_tags(agent, client)) == 1  # a span ignores a second finish; a count would not


def test_a_close_that_raises_still_records_and_reraises(traced: FakeTraceAgent) -> None:
    class Body:
        def __iter__(self) -> Iterator[bytes]:
            return iter([b"a"])

        def close(self) -> None:
            raise OSError("close failed")

    def app(environ: Any, start_response: Any) -> Body:
        start_response("200 OK", [])
        return Body()

    with pytest.raises(OSError, match="close failed"):
        run(TraceMiddleware(app))
    span = request_span(traced)
    assert span["error"] == 1
    assert span["meta"]["error.type"] == "OSError"
    assert span["meta"]["http.status_code"] == "200"  # the body had gone out


def test_a_mount_prefix_is_in_the_url_and_the_normalized_resource(
    traced: FakeTraceAgent,
) -> None:
    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        return [b"ok"]

    run(TraceMiddleware(app), path="/users/123", SCRIPT_NAME="/admin", QUERY_STRING=SECRET)
    span = request_span(traced)
    assert span["meta"]["http.url"] == "/admin/users/123"
    assert span["resource"] == "GET /admin/users/:id"
    assert SECRET not in str(traced.raw)


def test_an_adapter_can_name_a_swallowed_error(traced: FakeTraceAgent) -> None:
    def app(environ: Any, start_response: Any) -> list[bytes]:
        environ[ERROR_KEY] = LookupError("swallowed by the framework")
        start_response("500 Internal Server Error", [])
        return [b"oops"]

    run(TraceMiddleware(app))
    span = request_span(traced)
    assert span["meta"]["error.type"] == "LookupError"
    assert span["meta"]["error.message"] == "swallowed by the framework"


def test_a_swallowed_error_on_a_non_5xx_is_not_an_error(traced: FakeTraceAgent) -> None:
    def app(environ: Any, start_response: Any) -> list[bytes]:
        environ[ERROR_KEY] = LookupError("handled")
        start_response("404 Not Found", [])
        return [b""]

    run(TraceMiddleware(app))
    assert request_span(traced)["error"] == 0


def test_a_route_that_is_not_a_string_is_unmatched(traced: FakeTraceAgent) -> None:
    def app(environ: Any, start_response: Any) -> list[bytes]:
        environ[ROUTE_KEY] = 42
        start_response("200 OK", [])
        return [b""]

    run(TraceMiddleware(app), path="/a/99")
    span = request_span(traced)
    assert span["resource"] == "GET /a/:id"
    assert "http.route" not in span["meta"]


def test_a_missing_method_and_path_are_other_and_root(traced: FakeTraceAgent) -> None:
    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        return [b""]

    body = TraceMiddleware(app)({}, lambda *a: lambda data: None)
    list(body)
    body.close()  # type: ignore[attr-defined]
    span = request_span(traced)
    assert span["resource"] == "OTHER /"
    assert span["meta"]["http.url"] == ""


def test_a_recording_failure_never_reaches_the_request(
    make_client: ClientFactory, monkeypatch: pytest.MonkeyPatch
) -> None:
    client = make_client()

    def broken(*a: Any, **k: Any) -> None:
        raise RuntimeError("statsd is broken")

    monkeypatch.setattr(client, "increment", broken)

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        return [b"fine"]

    assert run(MetricsMiddleware(app, client=client))[1] == b"fine"


def test_custom_metric_names(make_client: ClientFactory, agent: FakeAgent) -> None:
    client = make_client()

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        return [b""]

    run(MetricsMiddleware(app, client=client, count_name="web.n", duration_name="web.ms"))
    assert [(n, k) for n, k, _ in metrics(agent, client)] == [("web.n", "c"), ("web.ms", "d")]


def test_a_disabled_middleware_returns_the_apps_body_untouched() -> None:
    body = [b"untouched"]

    def app(environ: Any, start_response: Any) -> list[bytes]:
        start_response("200 OK", [])
        return body

    assert TraceMiddleware(app)({"PATH_INFO": "/"}, lambda *a: lambda data: None) is body
