"""arq: carry a trace across the Redis queue, from the request that enqueued a job to the job.

Mental model
------------
HTTP has headers; a job queue has only the job's arguments. arq pickles
``enqueue_job(function, *args, **kwargs)`` into Redis and a worker, in another process,
calls ``function(ctx, *args, **kwargs)`` later. So the trace context rides in a reserved
keyword argument, ``_ozymandias``, and the two ends agree on it::

    API process                                         worker process
    ───────────                                         ──────────────
    http.request
      └─ arq.enqueue  (producer)  ── _ozymandias ──►    arq.job  (consumer)
         kwargs += {trace_id,                            pops the kwarg, extracts it,
                    parent_id,                           starts a child span, calls your
                    sampling_priority}                   function WITHOUT it

The ``arq.enqueue`` span is the *parent* of ``arq.job``: its id is the ``parent_id`` in
the carrier. The gap between the two spans is the time the job sat in the queue, and
the job span also records it as the metric ``queue.wait_ms`` (from arq's own
``ctx["enqueue_time"]``), so a slow queue is a number and not a guess.

The carrier is plain ``str``/``int`` values in a ``dict``: it pickles on any
arq serializer and survives a round trip unchanged (``extract(inject(ctx)) == ctx``).

Two halves, and the order they ship in matters
----------------------------------------------
``inject_job_kwargs`` (or the ``patch()``, which applies it to every ``enqueue_job``) adds
a kwarg your job functions do not declare; :func:`traced` / :func:`traced_job` is what
removes it. **Wrap the worker's functions and deploy workers before the API**, or a worker
without the wrapper calls ``fn(ctx, _ozymandias={...})`` and the job fails with a
``TypeError``. A job enqueued by an older, untraced API (no carrier) still runs under a
traced worker: that is the legacy case and it simply starts a new root trace.

Cron jobs have no enqueuer, so they carry no context and start a new root trace per
run. An ``arq.Retry`` is a request to run again, not a failure, so it does not mark the
span as an error.

Rejected alternative: patching ``Worker.run_job`` to strip the kwarg automatically. That
would make a missed wrapper harmless, but it patches a long internal method whose body
changes between arq releases; two explicit, small, public helpers are easier to trust.
"""

from __future__ import annotations

import dataclasses
import functools
import logging
import time
from collections.abc import Callable
from typing import Any

from .. import tracer as _tracer
from .._tracer import NOOP_SPAN
from .._tracing import Context
from . import register_integration
from ._patching import PatchSet, active_span

_log = logging.getLogger("ozy")

CARRIER_KWARG = "_ozymandias"
"""The reserved job kwarg that carries the trace context."""


def _carrier(context: Context) -> dict[str, Any]:
    return {
        "trace_id": context.trace_id,
        "parent_id": context.span_id,
        "sampling_priority": context.sampling_priority,
    }


def inject_job_kwargs(kwargs: dict[str, Any], *, function: str | None = None) -> dict[str, Any]:
    """Return ``kwargs`` plus the ``_ozymandias`` carrier, inside an ``arq.enqueue`` span.

    Use it when you enqueue by hand and have not called ``patch()``::

        await pool.enqueue_job("judge", **inject_job_kwargs({"submission_id": sid}))

    The span is started and finished here, so it is an instant: it marks *when* the job
    was enqueued and is the parent of the job's span, but it does not time the Redis
    write. ``patch()`` wraps ``enqueue_job`` itself and times the real call, which is
    why it is the better choice. Returns ``kwargs`` unchanged (a new dict, never the
    caller's) when tracing is disabled. Never raises.
    """
    out = dict(kwargs)
    try:
        span = _tracer.start_span(
            "arq.enqueue",
            resource=function or "arq.enqueue",
            type="queue",
            tags={"span.kind": "producer"},
            activate=False,
        )
        if span.span_id.strip("0"):
            out[CARRIER_KWARG] = _carrier(span.context)
        span.finish()
    except Exception:
        _log.debug("ozy: injecting job kwargs failed", exc_info=True)
    return out


def _wait_ms(enqueue_time: Any, now: float) -> float | None:
    try:
        wait = float((now - enqueue_time.timestamp()) * 1000.0)
    except Exception:
        return None
    return max(0.0, wait)


def traced[F](func: F) -> F:
    """Wrap an arq job so it runs inside an ``arq.job`` span continuing the enqueuer's trace.

    Accepts a plain coroutine function, an ``arq.func(...)`` ``Function`` or an
    ``arq.cron(...)`` ``CronJob``; for the last two the *options* (timeout, max_tries,
    name, schedule, ...) are preserved and only the coroutine is wrapped, because arq
    reads them off the object. For a coroutine function ``__name__`` and the signature
    are preserved too, so arq's default job name (the function's qualified name) is
    unchanged.

    Inside, in this order: the ``_ozymandias`` kwarg is *popped* (your function never
    sees it, so it cannot fail on an argument it does not declare), a context is
    extracted from it (none means a new root trace: cron jobs, legacy enqueuers), and
    the ``arq.job`` span is started as that context's child, type ``worker``, resource
    the function name, ``span.kind=consumer``, tagged ``job.id`` and ``job.try`` with the
    metric ``queue.wait_ms``. An exception marks the span and propagates unchanged.
    """
    if getattr(func, "__ozy_traced__", False):
        return func  # already wrapped: a second wrapper would nest a spurious root job span
    if dataclasses.is_dataclass(func) and not isinstance(func, type) and hasattr(func, "coroutine"):
        # An arq Function / CronJob: wrap its coroutine, keep every option.
        if getattr(func.coroutine, "__ozy_traced__", False):
            return func
        return dataclasses.replace(
            func, coroutine=_wrap(func.coroutine, getattr(func, "name", None))
        )
    return _wrap(func)  # type: ignore[no-any-return]


traced_job = traced
"""Alias of :func:`traced`, for decorator use: ``@traced_job`` above ``async def job(ctx, ...)``."""


def _wrap(func: Any, name: str | None = None) -> Any:
    # arq names a job by ``coroutine.__qualname__`` unless told otherwise, so that is the
    # resource: it is what the enqueuer called, and what the queue lists.
    job_name = name or getattr(func, "__qualname__", None) or "arq.job"

    @functools.wraps(func)
    async def wrapper(ctx: Any, *args: Any, **kwargs: Any) -> Any:
        # Pop first and unconditionally: whatever happens to tracing, the job must not
        # receive the carrier.
        carrier = kwargs.pop(CARRIER_KWARG, None)
        span = _start_job_span(ctx, job_name, carrier)
        try:
            result = await func(ctx, *args, **kwargs)
        except Exception as exc:
            if type(exc).__name__ == "Retry":
                # arq.Retry asks to run the job again later: control flow, not a failure.
                span.set_tag("job.retry", True)
            else:
                span.set_error(exc)
            span.finish()
            raise
        except BaseException:
            span.finish()  # cancellation or shutdown: end the span, do not call it an error
            raise
        span.finish()
        return result

    wrapper.__ozy_traced__ = True  # type: ignore[attr-defined]
    return wrapper


def _start_job_span(ctx: Any, name: str, carrier: Any) -> Any:
    try:
        parent = _tracer.extract(carrier) if isinstance(carrier, dict) else None
        span = _tracer.start_span(
            "arq.job",
            resource=name,
            type="worker",
            tags={"span.kind": "consumer"},
            child_of=parent,
        )
        if isinstance(ctx, dict):
            span.set_tag("job.id", ctx.get("job_id"))
            span.set_tag("job.try", ctx.get("job_try"))
            wait = _wait_ms(ctx.get("enqueue_time"), time.time())
            if wait is not None:
                span.set_metric("queue.wait_ms", wait)
        return span
    except Exception:
        _log.debug("ozy: starting the job span failed", exc_info=True)
        return NOOP_SPAN


def _wrap_enqueue(original: Callable[..., Any]) -> Callable[..., Any]:
    @functools.wraps(original)
    async def enqueue_job(self: Any, function: str, *args: Any, **kwargs: Any) -> Any:
        if active_span() is None:  # outside a trace there is nothing to continue
            return await original(self, function, *args, **kwargs)
        span = _tracer.start_span(
            "arq.enqueue",
            resource=str(function),
            type="queue",
            tags={"span.kind": "producer"},
            activate=False,
        )
        try:
            if span.span_id.strip("0"):
                kwargs[CARRIER_KWARG] = _carrier(span.context)
        except Exception:
            _log.debug("ozy: injecting job kwargs failed", exc_info=True)
        try:
            job = await original(self, function, *args, **kwargs)
        except Exception as exc:
            span.set_error(exc)
            span.finish()
            raise
        try:
            job_id = getattr(job, "job_id", None)
            if job_id:
                span.set_tag("job.id", job_id)
        finally:
            span.finish()
        return job

    return enqueue_job


class ArqIntegration:
    """Inject the carrier on every ``ArqRedis.enqueue_job`` made inside a trace.

    ``patch()`` handles the producer half only; the consumer half is :func:`traced`
    on the worker's functions (see the module docstring for why they are separate and
    which to deploy first).
    """

    name = "arq"

    def __init__(self) -> None:
        """Create an unpatched integration."""
        self._patches = PatchSet()

    def is_available(self) -> bool:
        """The arq package is importable."""
        try:
            import arq.connections  # noqa: F401
        except ImportError:
            return False
        return True

    def patch(self) -> None:
        """Wrap ``ArqRedis.enqueue_job``."""
        if self._patches.active or not self.is_available():
            return
        from arq.connections import ArqRedis

        self._patches.wrap(ArqRedis, "enqueue_job", _wrap_enqueue)

    def unpatch(self) -> None:
        """Restore ``ArqRedis.enqueue_job``."""
        self._patches.undo()


INTEGRATION = ArqIntegration()
register_integration(INTEGRATION)

__all__ = [
    "CARRIER_KWARG",
    "ArqIntegration",
    "inject_job_kwargs",
    "traced",
    "traced_job",
]
