"""httpx: one ``http.client`` span per outgoing request, with the trace headers injected.

Mental model
------------
``Client.send`` / ``AsyncClient.send`` is the one door every request leaves through
(``get``, ``post``, ``request``, redirects and all). Wrapping it gives a span for the
call and a place to write the propagation headers, so the service on the other end
continues the same trace::

    http.request (this service)
      └─ http.client  GET payments.internal     ── x-ozy-trace-id / -parent-id / -priority ──►
                                                     http.request (that service, same trace)

The span is type ``http``, resource ``"<METHOD> <host>"`` (the host, not the path:
paths carry ids and the host is what a service map wants), with ``http.method``,
``http.url`` (scheme, host and path only: no query string and no credentials, either of
which can hold secrets) and ``http.status_code``. A 5xx answer marks it as an error.

Headers are injected for every host by default. A trace id is not a secret, but it is
a correlation handle you may not want to hand to a third party: set
``INTEGRATION.inject_hosts = {"payments.internal"}`` to restrict injection to an
allow-list (the span is recorded either way).

Only inside a trace, like every client integration.
"""

from __future__ import annotations

import functools
import logging
from collections.abc import Callable, Iterable
from typing import Any

from .. import tracer as _tracer
from . import register_integration
from ._patching import PatchSet, active_span

_log = logging.getLogger("ozy")


def _safe_url(request: Any) -> str:
    url = request.url
    host = url.host
    port = f":{url.port}" if url.port else ""
    return f"{url.scheme}://{host}{port}{url.path}"


class HttpxIntegration:
    """Trace ``httpx.Client`` and ``httpx.AsyncClient`` requests.

    Attributes:
        inject_hosts: ``None`` (default) injects the propagation headers into every
            outgoing request; a set of hostnames injects only for those.
    """

    name = "httpx"

    def __init__(self) -> None:
        """Create an unpatched integration."""
        self._patches = PatchSet()
        self.inject_hosts: Iterable[str] | None = None

    def is_available(self) -> bool:
        """The httpx package is importable."""
        try:
            import httpx  # noqa: F401
        except ImportError:
            return False
        return True

    def _begin(self, request: Any) -> Any:
        span = _tracer.start_span(
            "http.client",
            resource=f"{request.method} {request.url.host}",
            type="http",
            tags={
                "http.method": request.method,
                "http.url": _safe_url(request),
                "span.kind": "client",
            },
            activate=False,
        )
        hosts = self.inject_hosts
        if hosts is None or request.url.host in hosts:
            _tracer.inject(request.headers, span.context)
        return span

    @staticmethod
    def _end(span: Any, response: Any) -> None:
        code = response.status_code
        span.set_tag("http.status_code", code)
        if code >= 500:
            span.set_error()

    def _wrap_async(self, original: Callable[..., Any]) -> Callable[..., Any]:
        @functools.wraps(original)
        async def send(client: Any, request: Any, *args: Any, **kwargs: Any) -> Any:
            if active_span() is None:
                return await original(client, request, *args, **kwargs)
            span = self._begin(request)
            try:
                response = await original(client, request, *args, **kwargs)
            except Exception as exc:
                span.set_error(exc)
                span.finish()
                raise
            self._end(span, response)
            span.finish()
            return response

        return send

    def _wrap_sync(self, original: Callable[..., Any]) -> Callable[..., Any]:
        @functools.wraps(original)
        def send(client: Any, request: Any, *args: Any, **kwargs: Any) -> Any:
            if active_span() is None:
                return original(client, request, *args, **kwargs)
            span = self._begin(request)
            try:
                response = original(client, request, *args, **kwargs)
            except Exception as exc:
                span.set_error(exc)
                span.finish()
                raise
            self._end(span, response)
            span.finish()
            return response

        return send

    def patch(self) -> None:
        """Wrap ``Client.send`` and ``AsyncClient.send``."""
        if self._patches.active or not self.is_available():
            return
        import httpx

        self._patches.wrap(httpx.AsyncClient, "send", self._wrap_async)
        self._patches.wrap(httpx.Client, "send", self._wrap_sync)

    def unpatch(self) -> None:
        """Restore the original methods."""
        self._patches.undo()


INTEGRATION = HttpxIntegration()
register_integration(INTEGRATION)
