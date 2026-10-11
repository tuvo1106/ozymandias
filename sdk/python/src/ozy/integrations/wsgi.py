"""WSGI middleware and the Flask integration: RED metrics and one span per request.

Mental model
------------
The ASGI middleware (:mod:`ozy.integrations.asgi`) for the other half of the Python web
world. A WSGI app is ``app(environ, start_response) -> iterable of bytes``, and the
request is *not over* when the call returns: the server iterates the body and then must
call its ``close()`` (PEP 3333). A streamed response, or a Flask teardown, runs during
that iteration. So the middleware returns a wrapped body and records the request in its
``close()``, not when ``app()`` returns; recording at return would time a streamed
download as a few microseconds and finish the span before its children.

The status is read from ``start_response``, which an app may call more than once (a
second call with ``exc_info`` replaces the status of a response whose headers were not
sent yet), so the last call that succeeded wins. Headers go out with the first non-empty
body chunk, so that is when the client has been answered: if the app fails before it,
the server sends a 500 and that is what is recorded, whatever status the app had set.
After it, the status already sent stands and the span is marked failed.

The route is the framework's business, not the middleware's, because routing happens
inside the app. A framework adapter writes the matched pattern into
``environ["ozy.route"]`` (:data:`ROUTE_KEY`) while it dispatches, and the middleware
reads it after the app has run, which is the same trick the ASGI middleware plays with
Starlette's ``scope["route"]``. The :class:`FlaskIntegration` below is such an adapter;
one for Django or Bottle is the same few lines. With no adapter every request is
``route:unmatched`` and the span resource falls back to the normalized raw path, so the
counts are right and the per-route breakdown is not.

A framework that turns an exception into a 500 page swallows it, so the middleware never
sees why the request failed. An adapter can put the exception in ``environ["ozy.error"]``
(:data:`ERROR_KEY`) and a 5xx span then carries its type and message.

Emitted metrics (the same names and bounded tag set as the ASGI middleware)::

    http.request.count     c  tags: route, method, status, status_class
    http.request.duration  d  milliseconds, same tags

Limits, all from WSGI itself:

- No ``499``. WSGI has no disconnect event; a client that goes away shows up, if at all,
  as an ``OSError`` from a write inside the server, after the app's part is done. The
  request is recorded with the status that was being sent.
- The body is wrapped, so the server's ``wsgi.file_wrapper`` fast path (``sendfile``)
  is not taken for responses that use it. Correct, slower for big static files; exclude
  those paths, or serve them from something that is not the app.
- Context follows the request only within one thread, as WSGI servers run a request:
  a span started in a thread the handler spawns needs ``tracer.wrap_executor``.

Rejected alternative: finishing the span when ``app()`` returns and ignoring the body.
It is simpler and is right for most JSON endpoints, but it silently mis-times every
streamed or file response and drops whatever the framework does in ``close()``.
"""

from __future__ import annotations

import functools
import logging
import time
from collections.abc import Callable, Iterable, Iterator
from typing import Any

from .. import StatsdClient, Tracer
from .. import statsd as _default_client
from .. import tracer as _default_tracer
from .._tracing import HEADER_PARENT_ID, HEADER_PRIORITY, HEADER_TRACE_ID, normalize_path
from . import register_integration
from ._patching import PatchSet
from .asgi import METHODS, OTHER, SERVER_ERROR, UNMATCHED, _status_code

Environ = dict[str, Any]
StartResponse = Callable[..., Callable[[bytes], object]]
WSGIApp = Callable[[Environ, StartResponse], Iterable[bytes]]

_log = logging.getLogger("ozy")

ROUTE_KEY = "ozy.route"
"""``environ`` key a framework adapter sets to the matched route pattern (a ``str``)."""

ERROR_KEY = "ozy.error"
"""``environ`` key a framework adapter sets to an exception it turned into a 5xx page."""

# Set by the first (outermost) middleware to see a request, so a second one on the same
# request passes it through: an app wrapped by hand *and* by the flask integration is
# counted once.
_SEEN_KEY = "ozy.wsgi"

# The propagation headers as WSGI spells them: ``x-ozy-trace-id`` -> ``HTTP_X_OZY_TRACE_ID``.
_ENVIRON_HEADERS = {
    "HTTP_" + name.upper().replace("-", "_"): name
    for name in (HEADER_TRACE_ID, HEADER_PARENT_ID, HEADER_PRIORITY)
}


class MetricsMiddleware:
    """Record ``http.request.count`` and ``http.request.duration`` per WSGI request.

    Never raises into the request: a failure while recording is logged at debug level
    and dropped. The app's own exceptions pass through untouched.

    Args:
        app: The WSGI application to wrap.
        client: The statsd client to send to. Defaults to the process-wide
            ``ozy.statsd``, which is a no-op until ``ozy.init()`` enables it.
        exclude_paths: Exact paths to leave unrecorded (health checks), matched against
            ``environ["PATH_INFO"]``, so under a ``SCRIPT_NAME`` mount it is the path
            inside the mount.
        count_name: Metric name for the counter.
        duration_name: Metric name for the duration distribution.
    """

    def __init__(
        self,
        app: WSGIApp,
        *,
        client: StatsdClient | None = None,
        exclude_paths: Iterable[str] = (),
        count_name: str = "http.request.count",
        duration_name: str = "http.request.duration",
    ) -> None:
        """Wrap ``app``; see the class docstring for the arguments."""
        self.app = app
        self._client = client if client is not None else _default_client
        # A bare string is one path, not an iterable of characters (see the ASGI middleware).
        paths = (exclude_paths,) if isinstance(exclude_paths, str) else exclude_paths
        self._exclude = frozenset(paths)
        self._count_name = count_name
        self._duration_name = duration_name

    def __call__(self, environ: Environ, start_response: StartResponse) -> Iterable[bytes]:
        """Run the wrapped app; the request is recorded when the server closes the body."""
        if (
            environ.get(_SEEN_KEY)
            or not self._active()
            or environ.get("PATH_INFO") in self._exclude
        ):
            return self.app(environ, start_response)
        environ[_SEEN_KEY] = True

        state = self._begin(environ)
        started = time.perf_counter()
        # The raw status line from the app, parsed only inside ``_end``'s guard: an app that
        # sends a bad one must not break its own response because we tried to read it.
        status: Any = None
        answered = False  # a non-empty body chunk went out, and the headers with it
        finished = False

        def finish(failure: BaseException | None) -> None:
            nonlocal finished
            if finished:
                return
            finished = True
            # A failure before the headers went out is answered with a 500 by the server.
            line = status if failure is None or answered else None
            self._end(state, environ, line, (time.perf_counter() - started) * 1000.0, failure)

        def sent() -> None:
            nonlocal answered
            answered = True

        def start_response_wrapper(
            status_line: str, headers: list[tuple[str, str]], exc_info: Any = None
        ) -> Callable[[bytes], object]:
            nonlocal status
            # Only after the server accepted it: a second call after the headers were sent
            # re-raises ``exc_info`` and changes nothing the client sees.
            # ``exc_info`` is forwarded only when there is one: a server's two-argument
            # ``start_response`` must not see a third.
            write = (
                start_response(status_line, headers)
                if exc_info is None
                else start_response(status_line, headers, exc_info)
            )
            status = status_line

            def write_wrapper(data: bytes) -> object:
                result = write(data)
                if data:
                    sent()
                return result

            return write_wrapper

        try:
            result = self.app(environ, start_response_wrapper)
        except BaseException as exc:
            finish(exc)
            raise
        return _Body(result, sent, finish)

    # The same two hooks as the ASGI middleware: ``_begin`` before the app (a tracing
    # subclass starts its span), ``_end`` once the body is closed. Must not raise.
    def _active(self) -> bool:
        return self._client.enabled

    def _begin(self, environ: Environ) -> Any:
        return None

    def _end(
        self,
        state: Any,
        environ: Environ,
        status: Any,
        elapsed_ms: float,
        failure: BaseException | None,
    ) -> None:
        self._record(environ, status, elapsed_ms)

    def _record(self, environ: Environ, status: Any, elapsed_ms: float) -> None:
        try:
            code = _status_code(_parse_status(status))
            tags = [
                f"route:{_route_pattern(environ)}",
                f"method:{_method(environ)}",
                f"status:{code}",
                f"status_class:{code // 100}xx",
            ]
            self._client.increment(self._count_name, tags=tags)
            self._client.distribution(self._duration_name, elapsed_ms, tags=tags)
        except Exception:
            _log.debug("ozy: recording an http request failed", exc_info=True)


class _Body:
    """The app's body, passed through chunk by chunk, that reports when it is done.

    A class with ``__next__`` rather than a generator: a generator wrapper would turn the
    server dropping the body early into a ``GeneratorExit`` at some later garbage
    collection, and recording would happen whenever that was.
    """

    __slots__ = ("_finish", "_it", "_result", "_sent")

    def __init__(
        self,
        result: Iterable[bytes],
        sent: Callable[[], None],
        finish: Callable[[BaseException | None], None],
    ) -> None:
        self._result = result
        self._sent = sent
        self._finish = finish
        self._it: Iterator[bytes] | None = None

    def __iter__(self) -> _Body:
        return self

    def __next__(self) -> bytes:
        try:
            if self._it is None:
                self._it = iter(self._result)
            chunk = next(self._it)
        except StopIteration:
            raise
        except BaseException as exc:
            self._finish(exc)
            raise
        if chunk:
            self._sent()
        return chunk

    def close(self) -> None:
        """Close the app's body (PEP 3333 says the server must), then record the request."""
        failure: BaseException | None = None
        try:
            close = getattr(self._result, "close", None)
            if close is not None:
                close()
        except BaseException as exc:
            failure = exc
            raise
        finally:
            self._finish(failure)
            failure = None  # the exception holds its traceback, which holds this frame


def _parse_status(status: Any) -> int | None:
    """The code of a WSGI status line (``"404 Not Found"``), or ``None`` if it is not one."""
    if not isinstance(status, str):
        return None
    head = status[:3]
    if len(status) >= 3 and head.isascii() and head.isdigit() and status[3:4] in ("", " "):
        return int(head)
    return None


def _route_pattern(environ: Environ) -> str:
    """The pattern a framework adapter stamped on the request, or ``unmatched``."""
    route = environ.get(ROUTE_KEY)
    return route if isinstance(route, str) and route else UNMATCHED


def _method(environ: Environ) -> str:
    """The request method if it is a standard verb, else ``OTHER``."""
    method = environ.get("REQUEST_METHOD")
    return method if method in METHODS else OTHER


def _path(environ: Environ) -> str:
    """The request path as the client sent it, mount prefix included, no query string."""
    script = environ.get("SCRIPT_NAME")
    path = environ.get("PATH_INFO")
    return (script if isinstance(script, str) else "") + (path if isinstance(path, str) else "")


class TraceMiddleware(MetricsMiddleware):
    """:class:`MetricsMiddleware` that also records an ``http.request`` span per request.

    The span is type ``web``, named ``http.request``, with resource
    ``"<METHOD> <route pattern>"`` (from :data:`ROUTE_KEY`; with none, the normalized
    path). Tags: ``http.method``, ``http.url`` (path only, no query string),
    ``http.route``, ``http.status_code``, ``span.kind=server``. The span is active while
    the app runs and while its body is iterated, so spans made by the handler, or by a
    streamed body, are its children. Propagation headers continue an upstream trace;
    malformed ones start a fresh one. A 5xx, or an exception, marks the span failed. Like
    its base it never raises into the request.

    Args:
        app: The WSGI application to wrap.
        client: The statsd client for the request metrics (default ``ozy.statsd``).
        tracer: The tracer for spans (default ``ozy.tracer``).
        exclude_paths: Exact ``PATH_INFO`` values to leave untraced and unrecorded.
        count_name: Metric name for the counter.
        duration_name: Metric name for the duration distribution.
    """

    def __init__(
        self,
        app: WSGIApp,
        *,
        client: StatsdClient | None = None,
        tracer: Tracer | None = None,
        exclude_paths: Iterable[str] = (),
        count_name: str = "http.request.count",
        duration_name: str = "http.request.duration",
    ) -> None:
        """Wrap ``app``; see the class docstring for the arguments."""
        super().__init__(
            app,
            client=client,
            exclude_paths=exclude_paths,
            count_name=count_name,
            duration_name=duration_name,
        )
        self._tracer = tracer if tracer is not None else _default_tracer

    def _active(self) -> bool:
        return self._client.enabled or self._tracer.enabled

    def _begin(self, environ: Environ) -> Any:
        try:
            if not self._tracer.enabled:
                return None
            carrier = {
                name: environ[key] for key, name in _ENVIRON_HEADERS.items() if key in environ
            }
            return self._tracer.start_span(
                "http.request",
                type="web",
                tags={"span.kind": "server"},
                child_of=self._tracer.extract(carrier),
            )
        except Exception:
            _log.debug("ozy: starting the request span failed", exc_info=True)
            return None

    def _end(
        self,
        state: Any,
        environ: Environ,
        status: Any,
        elapsed_ms: float,
        failure: BaseException | None,
    ) -> None:
        if state is not None:
            try:
                self._finish_span(state, environ, status, failure)
            except Exception:
                _log.debug("ozy: finishing the request span failed", exc_info=True)
        super()._end(state, environ, status, elapsed_ms, failure)

    @staticmethod
    def _finish_span(
        span: Any, environ: Environ, status: Any, failure: BaseException | None
    ) -> None:
        code = _status_code(_parse_status(status))
        method = _method(environ)
        route = _route_pattern(environ)
        path = _path(environ)
        span.resource = f"{method} {route if route != UNMATCHED else normalize_path(path)}"
        span.set_tag("http.method", method)
        span.set_tag("http.url", path)
        if route != UNMATCHED:
            span.set_tag("http.route", route)
        span.set_tag("http.status_code", code)
        swallowed = environ.get(ERROR_KEY)
        if isinstance(failure, Exception):
            span.set_error(failure)
        elif code >= SERVER_ERROR:
            span.set_error(swallowed if isinstance(swallowed, Exception) else None)
        span.finish()


class FlaskIntegration:
    """Trace every Flask app, with route patterns, with no code change.

    Three wraps on ``flask.Flask``, all on the class, so apps created before or after
    ``patch()`` are covered:

    - ``wsgi_app`` runs inside a :class:`TraceMiddleware`. It is the method Flask's
      ``__call__`` delegates to and the one ``app.wsgi_app = SomeMiddleware(app.wsgi_app)``
      wraps, so other WSGI middleware added that way sits outside it.
    - ``full_dispatch_request`` stamps ``request.url_rule.rule`` (``/items/<int:id>``,
      blueprint prefix included) into :data:`ROUTE_KEY` before the handler runs. A 404
      has no rule and stays ``unmatched``.
    - ``handle_exception``, which Flask calls for an unhandled error just before turning
      it into a 500 page, records the error in :data:`ERROR_KEY` for the span.

    An app that wraps itself in :class:`MetricsMiddleware` or :class:`TraceMiddleware`
    by hand is counted once: the outermost middleware claims the request.

    Attributes:
        exclude_paths: request paths left unrecorded, e.g. ``{"/healthz"}``; empty by
            default. Set it before the first request (``INTEGRATION.exclude_paths = ...``),
            as a load balancer's probes would otherwise be most of ``http.request.count``.
    """

    name = "flask"

    def __init__(self) -> None:
        """Create an unpatched integration."""
        self._patches = PatchSet()
        self.exclude_paths: Iterable[str] = ()

    def is_available(self) -> bool:
        """Flask is importable."""
        try:
            import flask  # noqa: F401
        except ImportError:
            return False
        return True

    def patch(self) -> None:
        """Wrap ``wsgi_app``, ``full_dispatch_request`` and ``handle_exception``."""
        if self._patches.active or not self.is_available():
            return
        from flask import Flask

        self._patches.wrap(Flask, "wsgi_app", _wrap_wsgi_app)
        self._patches.wrap(Flask, "full_dispatch_request", _wrap_dispatch)
        self._patches.wrap(Flask, "handle_exception", _wrap_handle_exception)

    def unpatch(self) -> None:
        """Restore the three methods."""
        self._patches.undo()


def _wrap_wsgi_app(original: Callable[..., Iterable[bytes]]) -> Callable[..., Iterable[bytes]]:
    @functools.wraps(original)
    def wsgi_app(self: Any, environ: Environ, start_response: StartResponse) -> Iterable[bytes]:
        try:
            # Built per request because the app is ``self``; construction is a few
            # attribute assignments, cheap next to a request.
            middleware = TraceMiddleware(
                functools.partial(original, self), exclude_paths=INTEGRATION.exclude_paths
            )
        except Exception:
            _log.debug("ozy: wrapping a flask request failed", exc_info=True)
            return original(self, environ, start_response)
        return middleware(environ, start_response)

    return wsgi_app


def _wrap_dispatch(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    def full_dispatch_request(self: Any, *args: Any, **kwargs: Any) -> Any:
        try:
            from flask import request

            rule = request.url_rule
            if rule is not None:
                request.environ[ROUTE_KEY] = rule.rule
        except Exception:
            _log.debug("ozy: reading the flask route failed", exc_info=True)
        return original(self, *args, **kwargs)

    return full_dispatch_request


def _wrap_handle_exception(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    def handle_exception(self: Any, e: Exception, *args: Any, **kwargs: Any) -> Any:
        try:
            from flask import request

            request.environ[ERROR_KEY] = e
        except Exception:
            _log.debug("ozy: recording the flask error failed", exc_info=True)
        return original(self, e, *args, **kwargs)

    return handle_exception


INTEGRATION = FlaskIntegration()
register_integration(INTEGRATION)
