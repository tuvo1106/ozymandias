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

The route tag is the *pattern* (``/api/v1/problems/{slug}``), never the raw
path, because a raw path with an id in it is one series per id. Routing has not
happened when the request arrives, so the pattern is read from
``scope["route"]`` after the inner app returns: Starlette and FastAPI stamp
the matched route onto the shared scope dict while dispatching. A request
that matched no route is tagged ``route:unmatched``, so a scanner probing
random URLs is one series, not thousands.

Emitted metrics (tag set is bounded by construction)::

    http.request.count     c  tags: route, method, status, status_class
    http.request.duration  d  milliseconds, same tags

Two cases the status tag has to be honest about. If the app raises before it
sent a response, the server will answer 500, so that is what is recorded. If
the client disconnects and the request is cancelled before a response started,
the status is ``499`` (nginx's "client closed request"): calling it 500 would
page someone for a user closing a tab.

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
            real traffic. Matched against the raw path, so they cost no series.
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
        self._exclude = frozenset(exclude_paths)
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
        status: int | None = None

        async def send_wrapper(message: Message) -> None:
            nonlocal status
            if message.get("type") == "http.response.start" and status is None:
                status = int(message.get("status", 0))
            await send(message)

        try:
            await self.app(scope, receive, send_wrapper)
        except asyncio.CancelledError:
            if status is None:
                status = CLIENT_CLOSED
            raise
        except BaseException:
            if status is None:
                status = 500
            raise
        finally:
            self._record(scope, status, (time.perf_counter() - started) * 1000.0)

    def _record(self, scope: Scope, status: int | None, elapsed_ms: float) -> None:
        try:
            # A response that never started and never raised (an app that
            # returned without answering) is a server fault from the client's side.
            code = 500 if status is None else status
            tags = [
                f"route:{_route_pattern(scope)}",
                f"method:{scope.get('method', 'UNKNOWN')}",
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
