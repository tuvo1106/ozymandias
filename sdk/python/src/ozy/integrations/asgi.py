"""ASGI middleware: one count and one duration per HTTP request.

Mental model
------------
A pure ASGI middleware wraps the app's ``(scope, receive, send)`` callable. It
sees the request go in and the ``http.response.start`` message come out, which
is everything needed for RED metrics (rate, errors, duration) with no
framework hooks. Pure ASGI rather than Starlette's ``BaseHTTPMiddleware``
because the latter runs the app in a separate task and breaks streaming
responses, background tasks and context variables; this wrapper adds one
``await`` and nothing else to the request's path.

The route tag is the *pattern* (``/problems/{slug}``), never the raw
path, because a raw path with an id in it is one series per id. Routing has not
happened when the request arrives, so the pattern is read from
``scope["route"]`` after the inner app returns: Starlette and FastAPI stamp
the matched route onto the shared scope dict while dispatching. The pattern
is relative to the router that matched, so a prefix given to FastAPI's
``include_router`` is not in it (verified against a live app, not assumed). A request
that matched no route is tagged ``route:unmatched``, so a scanner probing
random URLs is one series, not thousands.

Works with Starlette-family routers (Starlette, FastAPI), which put the matched route on
``scope["route"]``. Other ASGI frameworks never set it, so every request there is ``unmatched``:
the counts and durations are right but the per-route breakdown is not.

Emitted metrics (tag set is bounded by construction)::

    http.request.count     c  tags: route, method, status, status_class
    http.request.duration  d  milliseconds, same tags

Two cases the status tag has to be honest about. If the app raises before it
sent a response, the server will answer 500, so that is what is recorded. If
the request fails before a response started, a cancellation or any exception,
the client's behaviour decides: when its ``http.disconnect`` was seen on
``receive`` the status is ``499`` (nginx's "client closed request"), because
calling a closed tab a 500 would page someone for nothing. With no disconnect
seen, such as a shutdown, a reload or a timeout scope outside this middleware,
it is the server's doing and is recorded as 500. Even with a disconnect seen, only
a failure that looks like a closed connection is 499 (a cancellation, an ``OSError``, a
framework's ``ClientDisconnect``); any other exception is a handler bug and stays 500.
Limit: the disconnect is only seen if the app reads ``receive``, which a plain
non-streaming handler does not.

The ``method`` tag is limited to the standard verbs, anything else is
``OTHER``. A scanner can send any token as a method, and an unbounded tag is one
series per value, the same reason the route is capped at ``unmatched``.

Limits that cannot be fixed from inside a middleware, because the information is
not in the scope: the pattern is relative to the router that matched, so a
prefix given to ``include_router`` is not in it, and for a sub-app reached
through Starlette's ``Mount`` the route may be absent (``unmatched``) or
relative to the mount, depending on the Starlette version. Add the middleware
inside a mounted sub-app if its routes need exact patterns.

Rejected alternative: tagging the route in the middleware's *entry* from
``scope["path"]`` with an id-stripping regex. It needs a rule per app and
guesses wrong on slugs; the route table already knows the answer.
"""

from __future__ import annotations

import asyncio
import logging
import time
from collections.abc import Awaitable, Callable, Iterable, MutableMapping
from typing import Any

from .. import StatsdClient
from .. import statsd as _default_client

Scope = MutableMapping[str, Any]
Message = MutableMapping[str, Any]
Receive = Callable[[], Awaitable[Message]]
Send = Callable[[Message], Awaitable[None]]
ASGIApp = Callable[[Scope, Receive, Send], Awaitable[None]]

_log = logging.getLogger("ozy")

UNMATCHED = "unmatched"
CLIENT_CLOSED = 499
SERVER_ERROR = 500
OTHER = "OTHER"

# The methods worth a series of their own. Everything else is ``OTHER``: see the module docstring.
METHODS = frozenset({"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"})


class MetricsMiddleware:
    """Record ``http.request.count`` and ``http.request.duration`` per request.

    Never raises into the request: a failure while recording is logged at
    debug level and dropped. The app's own exceptions pass through untouched.

    Args:
        app: The ASGI application to wrap.
        client: The statsd client to send to. Defaults to the process-wide
            ``ozy.statsd``, which is a no-op until ``ozy.init()`` enables it.
        exclude_paths: Exact request paths to leave unrecorded, for health
            checks and other endpoints polled so often they would drown the
            real traffic. Matched exactly against the raw request path as the
            server reports it, so ``/healthz`` does not exclude ``/healthz/``,
            and behind a proxy that strips or adds a prefix the path to list is
            whatever ``scope["path"]`` holds, which differs between servers.
        count_name: Metric name for the counter.
        duration_name: Metric name for the duration distribution.
    """

    def __init__(
        self,
        app: ASGIApp,
        *,
        client: StatsdClient | None = None,
        exclude_paths: Iterable[str] = (),
        count_name: str = "http.request.count",
        duration_name: str = "http.request.duration",
    ) -> None:
        """Wrap ``app``; see the class docstring for the arguments."""
        self.app = app
        self._client = client if client is not None else _default_client
        # A bare string is one path, not an iterable of characters: ``frozenset("/healthz")``
        # would exclude "/" (the root route) and nothing else.
        paths = (exclude_paths,) if isinstance(exclude_paths, str) else exclude_paths
        self._exclude = frozenset(paths)
        self._count_name = count_name
        self._duration_name = duration_name

    async def __call__(self, scope: Scope, receive: Receive, send: Send) -> None:
        """Run the wrapped app, then record the request."""
        if scope.get("type") != "http" or not self._client.enabled:
            await self.app(scope, receive, send)
            return
        if scope.get("path") in self._exclude:
            await self.app(scope, receive, send)
            return

        started = time.perf_counter()
        # The raw value from the app's ``http.response.start``, coerced only inside ``_record``'s
        # guard: an app or middleware that sends a bad status must not break its own response
        # because we tried to read it.
        status: Any = None
        responded = False
        disconnected = False

        async def receive_wrapper() -> Message:
            nonlocal disconnected
            message = await receive()
            if message.get("type") == "http.disconnect":
                disconnected = True
            return message

        async def send_wrapper(message: Message) -> None:
            nonlocal status, responded
            if message.get("type") == "http.response.start" and not responded:
                # Only after the write succeeds: if it raises (the client closed the socket just
                # before), nothing reached the client, and the failure branch below classifies it.
                await send(message)
                responded = True
                status = message.get("status")
                return
            await send(message)

        try:
            await self.app(scope, receive_wrapper, send_wrapper)
        except BaseException as exc:
            # With no response started the client's disconnect decides, but only for failures
            # that look like a closed connection (a cancellation, an OSError from a write, a
            # framework's ClientDisconnect). A plain handler bug that happens to follow a
            # disconnect is still the server's fault and must reach the 5xx metrics.
            if not responded:
                status = CLIENT_CLOSED if disconnected and _client_gone(exc) else SERVER_ERROR
            raise
        finally:
            self._record(scope, status, (time.perf_counter() - started) * 1000.0)

    def _record(self, scope: Scope, status: Any, elapsed_ms: float) -> None:
        try:
            code = _status_code(status)
            tags = [
                f"route:{_route_pattern(scope)}",
                f"method:{_method(scope)}",
                f"status:{code}",
                f"status_class:{code // 100}xx",
            ]
            self._client.increment(self._count_name, tags=tags)
            self._client.distribution(self._duration_name, elapsed_ms, tags=tags)
        except Exception:
            _log.debug("ozy: recording an http request failed", exc_info=True)


def _route_pattern(scope: Scope) -> str:
    """The matched route's path template, or ``unmatched``."""
    route = scope.get("route")
    path = getattr(route, "path", None)
    return path if isinstance(path, str) and path else UNMATCHED


def _method(scope: Scope) -> str:
    """The request method if it is a standard verb, else ``OTHER`` (see the module docstring)."""
    method = scope.get("method")
    return method if method in METHODS else OTHER


def _status_code(status: Any) -> int:
    """A valid HTTP status, else 500.

    ``None`` means no response was started and nothing was raised, an app that returned without
    answering, which is a server fault from the client's side. A missing or non-numeric value
    from a misbehaving app gets the same answer instead of a new ``status:0`` series.
    """
    if isinstance(status, int) and not isinstance(status, bool) and 100 <= status <= 599:
        return status
    return SERVER_ERROR


# Exception types, by name, that mean "the connection went away" for backends this module does
# not import: Starlette's ``ClientDisconnect`` and trio/anyio's ``Cancelled``.
_CONNECTION_GONE_NAMES = frozenset({"ClientDisconnect", "Cancelled"})


def _client_gone(exc: BaseException) -> bool:
    """True for a cancellation or a closed-connection error, false for any other failure."""
    return (
        isinstance(exc, asyncio.CancelledError | OSError)
        or type(exc).__name__ in _CONNECTION_GONE_NAMES
    )
