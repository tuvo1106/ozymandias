"""Redis: one ``redis.command`` span per command or pipeline, named by the command only.

Mental model
------------
Every ``redis-py`` call funnels through ``execute_command`` (and a pipeline through its
``execute``), so wrapping those two methods on the client classes sees all of them,
sync and ``redis.asyncio``, with no change to application code.

What is recorded: the span is type ``cache``, the **resource is the command name**
(``GET``, ``HSET``) and nothing else. Keys and arguments are never read into the
span: a key is user data often enough (``session:<token>``, ``user:<email>``) that
"keys are fine" is a leak waiting to happen, and the command name already says what
kind of work it was. A pipeline is one span, resource ``PIPELINE``, with the metric
``redis.pipeline.commands``. A failure records the exception *type* only: redis error
messages quote arguments (``unknown command 'x', with args beginning with: ...``).

Only inside a trace, like every client integration: a command with no active span is
not traced, because a root span would count as a service entry in the agent's statistics.
This includes arq's own polling of its queue, which runs outside any request.
"""

from __future__ import annotations

import functools
import logging
from collections.abc import Callable
from typing import Any

from .. import tracer as _tracer
from . import register_integration
from ._patching import PatchSet, active_span

_log = logging.getLogger("ozy")

_MAX_COMMAND_CHARS = 32


def _command_name(args: tuple[Any, ...]) -> str:
    """The command's name, upper-cased: the only part of the call that is recorded."""
    try:
        first = args[0]
        if isinstance(first, bytes | bytearray):
            first = bytes(first).decode("ascii", "replace")
        return str(first).split(" ", 1)[0].upper()[:_MAX_COMMAND_CHARS] or "UNKNOWN"
    except Exception:
        return "UNKNOWN"


def _start(resource: str) -> Any:
    return _tracer.start_span(
        "redis.command",
        resource=resource,
        type="cache",
        tags={"db.system": "redis", "span.kind": "client"},
        activate=False,
    )


def _fail(span: Any, exc: BaseException) -> None:
    span.error = 1
    cls = type(exc)
    span.set_tag("error.type", f"{cls.__module__}.{cls.__qualname__}")


def _wrap_async_command(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    async def execute_command(self: Any, *args: Any, **options: Any) -> Any:
        if active_span() is None:
            return await original(self, *args, **options)
        span = _start(_command_name(args))
        try:
            return await original(self, *args, **options)
        except Exception as exc:
            _fail(span, exc)
            raise
        finally:
            span.finish()

    return execute_command


def _wrap_sync_command(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    def execute_command(self: Any, *args: Any, **options: Any) -> Any:
        if active_span() is None:
            return original(self, *args, **options)
        span = _start(_command_name(args))
        try:
            return original(self, *args, **options)
        except Exception as exc:
            _fail(span, exc)
            raise
        finally:
            span.finish()

    return execute_command


def _pipeline_span(pipeline: Any) -> Any:
    span = _start("PIPELINE")
    try:
        span.set_metric("redis.pipeline.commands", len(getattr(pipeline, "command_stack", ())))
    except Exception:
        _log.debug("ozy: counting pipeline commands failed", exc_info=True)
    return span


def _wrap_async_pipeline(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    async def execute(self: Any, *args: Any, **kwargs: Any) -> Any:
        if active_span() is None:
            return await original(self, *args, **kwargs)
        span = _pipeline_span(self)
        try:
            return await original(self, *args, **kwargs)
        except Exception as exc:
            _fail(span, exc)
            raise
        finally:
            span.finish()

    return execute


def _wrap_sync_pipeline(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    def execute(self: Any, *args: Any, **kwargs: Any) -> Any:
        if active_span() is None:
            return original(self, *args, **kwargs)
        span = _pipeline_span(self)
        try:
            return original(self, *args, **kwargs)
        except Exception as exc:
            _fail(span, exc)
            raise
        finally:
            span.finish()

    return execute


class RedisIntegration:
    """Trace ``redis`` and ``redis.asyncio`` commands and pipelines."""

    name = "redis"

    def __init__(self) -> None:
        """Create an unpatched integration."""
        self._patches = PatchSet()

    def is_available(self) -> bool:
        """redis-py is importable."""
        try:
            import redis.asyncio.client  # noqa: F401
        except ImportError:
            return False
        return True

    def patch(self) -> None:
        """Wrap ``execute_command`` and pipeline ``execute`` on the sync and asyncio clients."""
        if self._patches.active or not self.is_available():
            return
        import redis.asyncio.client as aclient
        import redis.client as sclient

        self._patches.wrap(aclient.Redis, "execute_command", _wrap_async_command)
        self._patches.wrap(aclient.Pipeline, "execute", _wrap_async_pipeline)
        self._patches.wrap(sclient.Redis, "execute_command", _wrap_sync_command)
        self._patches.wrap(sclient.Pipeline, "execute", _wrap_sync_pipeline)

    def unpatch(self) -> None:
        """Restore the original methods."""
        self._patches.undo()


INTEGRATION = RedisIntegration()
register_integration(INTEGRATION)
