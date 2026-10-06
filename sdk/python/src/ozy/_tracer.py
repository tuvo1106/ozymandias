"""The tracer: spans, in-process context, per-trace buffers and head sampling.

Mental model
------------
A **span** is one timed unit of work (``http.request``, ``postgres.query``,
``arq.job``) with an id, a parent id and the id of the **trace** it belongs to. The
tracer's whole job is to get those three ids right, in-process and across processes,
and to hand the finished spans to the writer::

    with tracer.trace("judge.run") as outer:        outer: trace T, span A, parent None
        with tracer.trace("docker.run") as inner:   inner: trace T, span B, parent A

In-process context lives in a ``contextvars.ContextVar`` holding the active span. That
is what makes the parent of a new span a *property of the call stack of the task*, not
of a global: it survives ``await`` (a coroutine keeps its context), is copied into
``asyncio.create_task`` (a child task starts with the spawner's active span, and
setting it there cannot disturb the spawner) and is **not** copied into threads (a
new thread starts with an empty context; use :meth:`Tracer.wrap_executor`).
Across processes the context travels as three values (:class:`Context`), in headers
or in a job's kwargs; see :meth:`Tracer.inject` and :meth:`Tracer.extract`.

Per-trace buffer
----------------
Spans do not go to the writer one by one. A trace's finished spans accumulate in a
buffer held by the **local root**, the first span this process saw for the trace
(no parent, or a parent that arrived over the wire). When the local root finishes the
buffer is handed to the writer as one **chunk**, which is what lets the agent see a
whole trace at once (error and rare-trace sampling look at every span of a chunk).
Two escape hatches keep memory bounded and nothing lost: a trace holding more than
500 finished spans flushes a *partial* chunk, and a span that finishes after its root
(a detached task) is sent as a *late* chunk of its own.

Head sampling
-------------
The keep/drop decision is made once, at the trace's first span, as a pure function of
the trace id (:func:`ozy._tracing.sample_keep`), so every service agrees without talking.
Dropped traces are **still sent** (``_sampling_priority = 0``): the agent computes
request, error and latency statistics over every span before it samples, and a 10%
sample must not show up as 10% of the traffic. Dropping happens in the agent.

Rejected alternatives: a lock per trace buffer (a fork while another thread holds it
deadlocks the child; one tracer-wide lock is reset by the after-fork hook, like the
statsd client's), and ``ContextVar.reset(token)`` to deactivate a span (it raises when
``finish()`` runs in a different context than ``start_span()``, which is normal for
callbacks; comparing against the active span cannot).

Invariants
----------
* **Nothing raises into the host.** Every entry point swallows its own failures; the
  one deliberate exception is user code inside ``trace()``/``wrap()``, whose
  exceptions propagate unchanged.
* **Disabled means inert.** Without an agent host (or with ``OZY_TRACE_ENABLED=0``)
  ``trace()`` returns a shared no-op span: your code runs, nothing is allocated,
  no thread, socket or exit hook exists.
* **The SDK never sends a span the agent would normalize or refuse**: limits from
  ``wire.NormalizeSpan`` are applied here (resource and meta values 5000 bytes, 100
  meta and 50 metrics, unknown type becomes ``custom``, ids lowercase hex, never zero).
"""

from __future__ import annotations

import atexit
import contextvars
import functools
import inspect
import logging
import math
import os
import platform
import threading
import time
import traceback
import weakref
from collections.abc import Callable, Mapping, MutableMapping
from concurrent.futures import Executor, Future
from dataclasses import dataclass
from types import TracebackType
from typing import Any, TypeVar, overload

from ._config import Config
from ._trace_writer import TraceWriter
from ._tracing import (
    HEADER_PARENT_ID,
    HEADER_PRIORITY,
    HEADER_TRACE_ID,
    PRIORITY_AUTO_DROP,
    PRIORITY_AUTO_KEEP,
    Context,
    carrier_get,
    parse_propagation,
    sample_keep,
)

__all__ = ["NoopSpan", "Span", "Tracer", "TracerStats"]

_log = logging.getLogger("ozy")

PARTIAL_FLUSH_SPANS = 500
"""A trace holding more finished spans than this flushes a partial chunk."""

SPAN_TYPES = frozenset({"web", "db", "cache", "queue", "http", "worker", "custom"})
MAX_RESOURCE_BYTES = 5000
MAX_META_VALUE_BYTES = 5000
MAX_KEY_BYTES = 100
MAX_META_ENTRIES = 100
MAX_METRIC_ENTRIES = 50
MAX_NAME_BYTES = 100
MAX_SERVICE_BYTES = 100

_RESERVED_METRICS = ("_sampling_priority", "_top_level", "_measured")

F = TypeVar("F", bound=Callable[..., Any])

_current: contextvars.ContextVar[Span | None] = contextvars.ContextVar(
    "ozy_current_span", default=None
)


@dataclass(frozen=True, slots=True)
class TracerStats:
    """Counters since ``init()``; the writer's three are zero until a chunk is sent.

    Attributes:
        spans_started: Spans created (no-op spans are not counted).
        spans_finished: Spans finished.
        chunks_sent: Chunks the agent answered ``200`` for.
        chunks_dropped: Chunks never delivered: evicted from a full queue, refused
            (``429``), failed to send, or too large.
        send_errors: Failed requests (connection errors, timeouts, non-200 answers).
    """

    spans_started: int = 0
    spans_finished: int = 0
    chunks_sent: int = 0
    chunks_dropped: int = 0
    send_errors: int = 0


class _TraceBuffer:
    """Finished spans of one trace in this process, owned by its local root."""

    __slots__ = ("priority", "root_finished", "spans")

    def __init__(self, priority: int) -> None:
        self.priority = priority
        self.spans: list[dict[str, Any]] = []
        self.root_finished = False


class Span:
    """One timed unit of work. Create with :meth:`Tracer.trace` or :meth:`Tracer.start_span`.

    A span is a context manager (sync and async): leaving the block finishes it,
    and an exception leaving the block marks it as an error and then propagates
    **unchanged**. Every method is safe to call on a finished span and none of
    them raises.

    Attributes:
        trace_id: 32 lowercase hex characters.
        span_id: 16 lowercase hex characters, never zero.
        parent_id: The parent's span id, or ``None`` for a trace root.
        name: Low-cardinality operation name (``http.request``).
        resource: What was operated on (``POST /items/{id}``, SQL text, job name).
        service: Service this span belongs to (defaults from ``init``).
        type: One of ``web db cache queue http worker custom``.
        error: ``1`` once marked as an error, else ``0``.
        meta: String tags.
        metrics: Numeric tags.
        duration_us: Microseconds, set at ``finish()``; ``None`` before.
    """

    __slots__ = (
        "_activated",
        "_buf",
        "_finished",
        "_is_root",
        "_prev",
        "_start_us",
        "_t0_ns",
        "_tracer",
        "duration_us",
        "error",
        "meta",
        "metrics",
        "name",
        "parent_id",
        "resource",
        "service",
        "span_id",
        "trace_id",
        "type",
    )

    def __init__(
        self,
        tracer: Tracer | None,
        buf: _TraceBuffer | None,
        *,
        trace_id: str,
        span_id: str,
        parent_id: str | None,
        name: str,
        resource: str,
        service: str,
        type: str,
        is_root: bool,
        start_us: int,
        t0_ns: int,
    ) -> None:
        """Internal: spans are created by the tracer."""
        self._tracer = tracer
        self._buf = buf
        self.trace_id = trace_id
        self.span_id = span_id
        self.parent_id = parent_id
        self.name = name
        self.resource = resource
        self.service = service
        self.type = type
        self.error = 0
        self.meta: dict[str, str] = {}
        self.metrics: dict[str, float] = {}
        self.duration_us: int | None = None
        self._is_root = is_root
        self._start_us = start_us
        self._t0_ns = t0_ns
        self._finished = False
        self._activated = False
        self._prev: Span | None = None

    @property
    def context(self) -> Context:
        """The :class:`Context` a child (in this or another process) needs."""
        buf = self._buf
        return Context(
            self.trace_id,
            self.span_id,
            buf.priority if buf is not None else PRIORITY_AUTO_KEEP,
        )

    @property
    def finished(self) -> bool:
        """Whether ``finish()`` has run."""
        return self._finished

    def set_tag(self, key: str, value: Any) -> None:
        """Set a string tag. ``None`` is ignored; other values are stringified."""
        try:
            if value is None or self._finished:
                return
            self.meta[str(key)] = _meta_value(value)
        except Exception:
            _log.debug("ozy: set_tag failed", exc_info=True)

    def set_metric(self, key: str, value: float) -> None:
        """Set a numeric tag. Non-finite or non-numeric values are ignored."""
        try:
            if self._finished:
                return
            number = float(value)
            if math.isfinite(number):
                self.metrics[str(key)] = number
        except Exception:
            _log.debug("ozy: set_metric failed", exc_info=True)

    def set_error(self, exc: BaseException | None = None) -> None:
        """Mark the span as an error, recording the exception's type, message and stack."""
        try:
            if self._finished:
                return
            self.error = 1
            if exc is not None:
                self.meta["error.type"] = _qualified_name(type(exc))
                self.meta["error.message"] = _safe_str(exc)
                self.meta["error.stack"] = "".join(traceback.format_exception(exc))
        except Exception:
            _log.debug("ozy: set_error failed", exc_info=True)

    def finish(self) -> None:
        """End the span. Idempotent: only the first call records anything."""
        tracer = self._tracer
        if tracer is None or self._finished:
            return
        try:
            tracer._finish(self)
        except Exception:
            _log.debug("ozy: finishing a span failed", exc_info=True)

    def __enter__(self) -> Span:
        """Enter the block; the span is already active (``trace()`` activates it)."""
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        """Finish the span; an ``Exception`` marks it as an error. Never swallows."""
        # Falsy return: the exception that left the block keeps propagating, unchanged.
        # A CancelledError is control flow, not a failure of the work, so it does not
        # mark the span.
        if isinstance(exc, Exception):
            self.set_error(exc)
        self.finish()

    async def __aenter__(self) -> Span:
        """Async twin of :meth:`__enter__`."""
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        """Async twin of :meth:`__exit__`."""
        self.__exit__(exc_type, exc, tb)


class NoopSpan(Span):
    """What ``trace()`` returns when tracing is disabled: same API, does nothing.

    A single shared instance. It is never activated, so ``current_span()`` stays
    ``None`` and code that branches on it behaves as if no trace were running.
    """

    __slots__ = ()

    def __init__(self) -> None:
        """Create the shared instance."""
        super().__init__(
            None,
            None,
            trace_id="0" * 32,
            span_id="0" * 16,
            parent_id=None,
            name="",
            resource="",
            service="",
            type="custom",
            is_root=False,
            start_us=0,
            t0_ns=0,
        )
        self._finished = True

    @property
    def context(self) -> Context:
        """A zero context: never propagated (``inject`` skips it)."""
        return Context("0" * 32, "0" * 16, PRIORITY_AUTO_DROP)

    def set_tag(self, key: str, value: Any) -> None:
        """Do nothing."""

    def set_metric(self, key: str, value: float) -> None:
        """Do nothing."""

    def set_error(self, exc: BaseException | None = None) -> None:
        """Do nothing."""

    def finish(self) -> None:
        """Do nothing."""

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        """Do nothing."""


NOOP_SPAN = NoopSpan()


class Tracer:
    """Creates spans, tracks the active one and ships finished traces to the agent.

    ``from ozy import tracer`` is the process-wide instance; ``ozy.init()``
    reconfigures it in place, so references taken before ``init()`` stay valid.
    """

    def __init__(
        self,
        *,
        wall_ns: Callable[[], int] = time.time_ns,
        mono_ns: Callable[[], int] = time.perf_counter_ns,
        random_bytes: Callable[[int], bytes] = os.urandom,
    ) -> None:
        """Create a disabled tracer. The clocks and id source are injectable for tests."""
        self._wall_ns = wall_ns
        self._mono_ns = mono_ns
        self._random_bytes = random_bytes
        self._config = Config()
        self._writer: TraceWriter | None = None
        self._rates: dict[str, float] = {}
        self._lock = threading.Lock()
        self._hooks_registered = False
        self.spans_started = 0
        self.spans_finished = 0

    # -- configuration -------------------------------------------------------------

    @property
    def enabled(self) -> bool:
        """Whether spans are recorded (an agent host is configured and tracing is on)."""
        return self._writer is not None

    def configure(self, config: Config) -> None:
        """Apply a configuration, replacing any previous one.

        The previous writer is drained first (within its exit budget). With
        ``config.traces_enabled`` false this leaves the tracer disabled and creates
        nothing: no thread, socket or hook.
        """
        self.close()
        self._config = config
        self._rates = {}
        self.spans_started = 0
        self.spans_finished = 0
        if not config.traces_enabled:
            return
        assert config.agent_host is not None
        from . import __version__

        self._writer = TraceWriter(
            config.agent_host,
            config.trace_port,
            tracer_info={
                "lang": "python",
                "lang_version": platform.python_version(),
                "version": __version__,
            },
            on_rates=self._set_rates,
        )
        self._register_process_hooks()

    def _register_process_hooks(self) -> None:
        # Once per tracer, and only once enabled: a disabled SDK leaves no trace in the
        # process. Weak references so a discarded tracer (common in tests) is collectable.
        if self._hooks_registered:
            return
        self._hooks_registered = True
        ref = weakref.ref(self)

        def at_exit() -> None:
            tracer = ref()
            if tracer is not None:
                tracer.close()

        def after_fork_in_child() -> None:
            tracer = ref()
            if tracer is not None:
                tracer._after_fork_in_child()

        atexit.register(at_exit)
        os.register_at_fork(after_in_child=after_fork_in_child)

    def _after_fork_in_child(self) -> None:
        # A lock held by another thread at fork time stays held forever in the child.
        self._lock = threading.Lock()
        writer = self._writer
        if writer is not None:
            writer.after_fork_in_child()

    def close(self) -> None:
        """Drain the writer (at most 1 s) and disable the tracer. Never raises."""
        try:
            writer, self._writer = self._writer, None
            if writer is not None:
                writer.close()
        except Exception:
            _log.debug("ozy: closing the tracer failed", exc_info=True)

    def flush(self) -> None:
        """Send every queued chunk now, on the calling thread. Never raises.

        Normally unnecessary; useful at the end of a short-lived job and in tests.
        """
        writer = self._writer
        if writer is not None:
            writer.flush()

    def stats(self) -> TracerStats:
        """A snapshot of the counters. Never raises.

        Counters are plain integers: exact on one thread, and possibly a count short
        when many threads finish spans at once. They are for noticing, not billing.
        """
        writer = self._writer
        return TracerStats(
            spans_started=self.spans_started,
            spans_finished=self.spans_finished,
            chunks_sent=writer.chunks_sent if writer else 0,
            chunks_dropped=writer.chunks_dropped if writer else 0,
            send_errors=writer.send_errors if writer else 0,
        )

    def _set_rates(self, rates: Mapping[str, Any]) -> None:
        clean: dict[str, float] = {}
        for key, value in rates.items():
            if isinstance(key, str) and isinstance(value, int | float) and value is not True:
                rate = float(value)
                if math.isfinite(rate):
                    clean[key] = min(1.0, max(0.0, rate))
        self._rates = clean  # one reference swap: readers never see a half-built dict

    def _rate_for(self, service: str) -> float:
        key = f"service:{service},env:{self._config.env or ''}"
        return self._rates.get(key, self._config.trace_sample_rate)

    # -- spans ---------------------------------------------------------------------

    def current_span(self) -> Span | None:
        """The active span of this task or thread, or ``None``."""
        return _active()

    def current_trace_context(self) -> Context | None:
        """The :class:`Context` of the active span, or ``None`` (what ``inject`` would write)."""
        span = _active()
        return span.context if span is not None else None

    def trace(
        self,
        name: str,
        *,
        resource: str | None = None,
        service: str | None = None,
        type: str = "custom",
        tags: Mapping[str, Any] | None = None,
        child_of: Context | Span | None = None,
    ) -> Span:
        """Start an active span for use as ``with`` / ``async with`` (and finish on exit).

        Usage::

            with tracer.trace("judge.run", resource=language, type="worker") as span:
                span.set_tag("problem.id", pid)

        Its parent is the active span (or ``child_of``); with neither it starts a new
        trace. Never raises; when tracing is disabled it returns a no-op span and your
        block still runs.
        """
        return self.start_span(
            name, resource=resource, service=service, type=type, tags=tags,
            child_of=child_of, activate=True,
        )  # fmt: skip

    def start_span(
        self,
        name: str,
        *,
        resource: str | None = None,
        service: str | None = None,
        type: str = "custom",
        tags: Mapping[str, Any] | None = None,
        child_of: Context | Span | None = None,
        activate: bool = True,
    ) -> Span:
        """The manual API: start a span and finish it yourself with ``span.finish()``.

        ``activate=False`` records the span without making it the parent of what
        happens next: right for a leaf (an outgoing call) that needs no children.
        Never raises; returns a no-op span when tracing is disabled.
        """
        if self._writer is None:
            return NOOP_SPAN
        try:
            return self._start(name, resource, service, type, tags, child_of, activate)
        except Exception:
            _log.debug("ozy: starting a span failed", exc_info=True)
            return NOOP_SPAN

    def _new_id(self, nbytes: int) -> str:
        while True:
            raw = self._random_bytes(nbytes)
            if any(raw):  # all-zero ids are invalid on the wire
                return raw.hex()

    def _start(
        self,
        name: str,
        resource: str | None,
        service: str | None,
        type: str,
        tags: Mapping[str, Any] | None,
        child_of: Context | Span | None,
        activate: bool,
    ) -> Span:
        parent: Span | None = None
        remote: Context | None = None
        if child_of is None:
            parent = _active()
        elif isinstance(child_of, Span):
            parent = child_of if child_of._buf is not None else None
        elif isinstance(child_of, Context):
            remote = child_of

        cfg = self._config
        if parent is not None:
            svc = service or parent.service
            trace_id, parent_id, buf = parent.trace_id, parent.span_id, parent._buf
            is_root = False
            top_level = svc != parent.service
        else:
            svc = service or cfg.service or "unknown"
            if remote is not None:
                trace_id, parent_id = remote.trace_id, remote.span_id
                priority = remote.sampling_priority
            else:
                trace_id, parent_id = self._new_id(16), None
                keep = sample_keep(trace_id, self._rate_for(svc))
                priority = PRIORITY_AUTO_KEEP if keep else PRIORITY_AUTO_DROP
            buf = _TraceBuffer(priority)
            is_root = True
            top_level = True

        span = Span(
            self,
            buf,
            trace_id=trace_id,
            span_id=self._new_id(8),
            parent_id=parent_id,
            name=name,
            resource=resource if resource is not None else name,
            service=svc,
            type=type,
            is_root=is_root,
            start_us=self._wall_ns() // 1000,
            t0_ns=self._mono_ns(),
        )
        if top_level:
            span.metrics["_top_level"] = 1.0
        if is_root:
            assert buf is not None
            span.metrics["_sampling_priority"] = float(buf.priority)
        if tags:
            for key, value in tags.items():
                span.set_tag(key, value)
        if activate:
            span._prev = _active()
            span._activated = True
            _current.set(span)
        self.spans_started += 1
        return span

    def _finish(self, span: Span) -> None:
        # Lock-free check-and-set: it only matters when two threads finish the *same*
        # span in the same instant, which is a bug in the caller; the buffer is what
        # needs the lock.
        if span._finished:
            return
        span._finished = True
        span.duration_us = max(0, (self._mono_ns() - span._t0_ns) // 1000)
        if span._activated:
            self._deactivate(span)
        self.spans_finished += 1
        buf = span._buf
        if buf is None:
            return
        wire = self._to_wire(span)
        chunk: list[dict[str, Any]] | None = None
        with self._lock:
            if span._is_root:
                buf.spans.append(wire)
                chunk, buf.spans = buf.spans, []
                buf.root_finished = True
            elif buf.root_finished:
                chunk = [wire]  # a late chunk: a detached task outlived the root
                _stamp_priority(wire, buf.priority)
            else:
                buf.spans.append(wire)
                if len(buf.spans) > PARTIAL_FLUSH_SPANS:
                    chunk, buf.spans = buf.spans, []
                    # The agent's priority sampler reads the chunk; a partial chunk has
                    # no root to carry the decision, so its first span does.
                    _stamp_priority(chunk[0], buf.priority)
        writer = self._writer
        if chunk is not None and writer is not None:
            writer.submit(chunk)

    @staticmethod
    def _deactivate(span: Span) -> None:
        if _current.get() is not span:
            return  # finished out of order, or from another context: leave the stack alone
        prev = span._prev
        while prev is not None and prev._finished:
            prev = prev._prev
        _current.set(prev)

    def _to_wire(self, span: Span) -> dict[str, Any]:
        """The span as a §B object, with the agent's normalization limits already applied."""
        cfg = self._config
        meta = dict(span.meta)
        if cfg.env and "env" not in meta:
            meta["env"] = cfg.env
        if cfg.version and "version" not in meta:
            meta["version"] = cfg.version
        span_type = span.type if span.type in SPAN_TYPES else "custom"
        return {
            "trace_id": span.trace_id,
            "span_id": span.span_id,
            "parent_id": span.parent_id,
            "service": _clip(span.service, MAX_SERVICE_BYTES) or "unknown",
            "name": _clip(span.name, MAX_NAME_BYTES) or "unnamed",
            "resource": _clip(span.resource, MAX_RESOURCE_BYTES),
            "type": span_type,
            "start": span._start_us,
            "duration": span.duration_us or 0,
            "error": 1 if span.error else 0,
            "meta": _limit(
                meta, MAX_META_ENTRIES, (), lambda v: _clip(_meta_value(v), MAX_META_VALUE_BYTES)
            ),
            # Finite only: one NaN written straight into ``span.metrics`` would make the
            # whole chunk unencodable, and a non-finite metric is refused by the agent.
            "metrics": _limit(
                {
                    k: float(v)
                    for k, v in span.metrics.items()
                    if isinstance(v, int | float) and math.isfinite(v)
                },
                MAX_METRIC_ENTRIES,
                _RESERVED_METRICS,
                float,
            ),
        }

    # -- wrapping ------------------------------------------------------------------

    @overload
    def wrap(self, name: F, /) -> F: ...

    @overload
    def wrap(
        self,
        name: str | None = None,
        *,
        resource: str | None = None,
        service: str | None = None,
        type: str = "custom",
        tags: Mapping[str, Any] | None = None,
    ) -> Callable[[F], F]: ...

    def wrap(
        self,
        name: Any = None,
        *,
        resource: str | None = None,
        service: str | None = None,
        type: str = "custom",
        tags: Mapping[str, Any] | None = None,
    ) -> Any:
        """Decorator: run every call of a function inside a span (sync and async).

        ``@tracer.wrap("cover.resize")`` or bare ``@tracer.wrap`` (the span is then
        named after the function). A generator function is traced only while it is
        being *created*, not across its yields: a span spanning ``yield`` would stay
        active in whoever iterates it.
        """
        if callable(name):
            return self._wrap(name, None, resource, service, type, tags)

        def decorator(func: F) -> F:
            return self._wrap(func, name, resource, service, type, tags)  # type: ignore[no-any-return]

        return decorator

    def _wrap(
        self,
        func: Any,
        name: str | None,
        resource: str | None,
        service: str | None,
        type: str,
        tags: Mapping[str, Any] | None,
    ) -> Any:
        span_name = name or getattr(func, "__qualname__", None) or "function"

        if inspect.iscoroutinefunction(func):

            @functools.wraps(func)
            async def async_wrapper(*args: Any, **kwargs: Any) -> Any:
                span = self.trace(
                    span_name, resource=resource, service=service, type=type, tags=tags
                )
                with span:
                    return await func(*args, **kwargs)

            return async_wrapper

        @functools.wraps(func)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            with self.trace(span_name, resource=resource, service=service, type=type, tags=tags):
                return func(*args, **kwargs)

        return wrapper

    def wrap_executor(self, executor: Executor) -> Executor:
        """Return an executor whose tasks run in the *submitter's* context.

        A thread starts with an empty context, so a span active where you call
        ``executor.submit(fn)`` is invisible inside ``fn`` and its spans would start
        new traces. The returned executor copies the context at submit time (what
        ``asyncio.to_thread`` does for you). ``loop.run_in_executor`` and ``map`` work
        on it too; everything else is the wrapped executor's.
        """
        return _ContextExecutor(executor)

    # -- propagation ---------------------------------------------------------------

    def inject(
        self, carrier: MutableMapping[str, str], context: Context | None = None
    ) -> MutableMapping[str, str]:
        """Write the propagation headers for ``context`` (default: the active span's).

        ``carrier`` is any mutable mapping: an outgoing request's headers, a dict.
        Nothing is written without a context (so a caller outside any trace sends no
        headers), and a failure leaves the carrier as it was. Returns the carrier.
        """
        try:
            ctx = context if context is not None else self.current_trace_context()
            if ctx is not None and self.enabled and ctx.span_id != "0" * 16:
                carrier[HEADER_TRACE_ID] = ctx.trace_id
                carrier[HEADER_PARENT_ID] = ctx.span_id
                carrier[HEADER_PRIORITY] = str(ctx.sampling_priority)
        except Exception:
            _log.debug("ozy: inject failed", exc_info=True)
        return carrier

    def extract(self, carrier: Mapping[Any, Any] | None) -> Context | None:
        """Read a :class:`Context` from headers or a job carrier; ``None`` if absent or bad.

        Accepts a header-like mapping (``x-ozy-*`` keys, any case, ``str`` or
        ``bytes``) or the dict form job queues carry (``trace_id``, ``parent_id``,
        ``sampling_priority``). A header that is malformed in any way yields ``None``
        and starts a fresh trace instead of continuing a corrupt one. Never raises.
        """
        try:
            if not isinstance(carrier, Mapping):
                return None
            trace_id = carrier_get(carrier, HEADER_TRACE_ID)
            if trace_id is not None:
                return parse_propagation(
                    trace_id,
                    carrier_get(carrier, HEADER_PARENT_ID),
                    carrier_get(carrier, HEADER_PRIORITY),
                )
            return parse_propagation(
                carrier.get("trace_id"), carrier.get("parent_id"), carrier.get("sampling_priority")
            )
        except Exception:
            return None


class _ContextExecutor(Executor):
    """An :class:`Executor` that runs each task in a copy of the submitter's context."""

    def __init__(self, inner: Executor) -> None:
        self._inner = inner

    def submit(self, fn: Callable[..., Any], /, *args: Any, **kwargs: Any) -> Future[Any]:
        ctx = contextvars.copy_context()
        return self._inner.submit(ctx.run, fn, *args, **kwargs)

    def shutdown(self, wait: bool = True, *, cancel_futures: bool = False) -> None:
        self._inner.shutdown(wait=wait, cancel_futures=cancel_futures)

    def __getattr__(self, name: str) -> Any:
        return getattr(self._inner, name)


def _active() -> Span | None:
    """The active span, skipping any that already finished.

    A span finished from another context (a callback, a thread) cannot deactivate itself
    in the context that activated it, so it can still be what the variable holds there.
    A finished span is never a useful parent for new work; its own saved parent is.
    """
    span = _current.get()
    while span is not None and span._finished:
        span = span._prev
    return span


# -- normalization helpers ---------------------------------------------------------


def _stamp_priority(wire: dict[str, Any], priority: int) -> None:
    wire["metrics"].setdefault("_sampling_priority", float(priority))


def _meta_value(value: Any) -> str:
    if isinstance(value, bool):
        return "true" if value else "false"
    return value if isinstance(value, str) else str(value)


def _safe_str(exc: BaseException) -> str:
    try:
        return str(exc)
    except Exception:
        return f"<unprintable {type(exc).__name__}>"


def _qualified_name(cls: type) -> str:
    module = cls.__module__
    return cls.__qualname__ if module == "builtins" else f"{module}.{cls.__qualname__}"


def _clip(text: str, limit: int) -> str:
    """Cut ``text`` to ``limit`` bytes of UTF-8 without splitting a character."""
    if len(text) * 4 <= limit:
        return text  # even at 4 bytes a character it fits
    raw = text.encode("utf-8", "replace")
    if len(raw) <= limit:
        return text
    return raw[:limit].decode("utf-8", "ignore")


def _limit[V](
    items: Mapping[str, V],
    max_entries: int,
    protected: tuple[str, ...],
    fix: Callable[[V], V],
) -> dict[str, V]:
    """Apply the agent's per-map limits: key length, then entry count in sorted key order.

    Dropping in sorted order makes the same span normalize the same way every time.
    ``protected`` keys (the ``_top_level`` family) survive the count limit: they are
    how the agent knows what to do with the span.
    """
    keep = {k: v for k, v in items.items() if k and len(k) <= MAX_KEY_BYTES}
    if len(keep) > max_entries:
        keys = [k for k in sorted(keep) if k not in protected]
        kept_protected = [k for k in protected if k in keep]
        room = max_entries - len(kept_protected)
        keep = {k: keep[k] for k in sorted([*kept_protected, *keys[: max(room, 0)]])}
    return {k: fix(v) for k, v in keep.items()}
