"""The statsd client: format on the caller's thread, send on a background thread.

Data path for one call, e.g. ``statsd.increment("page.views")``::

    caller thread                         flusher thread (daemon, lazy)
    ─────────────                         ─────────────────────────────
    sample? (random() < rate)
    format line (pure, _format.py)
    lock ─► append to buffer              every flush_interval, or when woken:
            buffer full? ─► pending ──wake──► lock ─► take pending (+ buffer on timer)
    unlock                                        unlock ─► UdpTransport.send()

The caller never does I/O. It formats a string, takes a lock for a few list
operations and returns, which is what lets instrumentation sit on hot paths.
Sending happens on one daemon thread that wakes every ``flush_interval``
(100 ms) or as soon as a buffer fills up.

Invariants:

* **Bounded memory.** The buffer never exceeds ``max_payload`` bytes and the
  queue of full payloads waiting to be sent holds at most
  ``MAX_PENDING_PAYLOADS``; when it overflows, the *oldest* payload is
  dropped (recent data is worth more) and its messages are counted as
  dropped. An unreachable agent can never grow the host's memory.
* **Nothing raises into the host app.** Every public method is wrapped;
  an internal failure increments ``errors`` and returns ``None``. The one
  deliberate exception is user code inside ``timed``, whose exceptions
  propagate unchanged.
* **Disabled means inert.** Without an agent host, no socket, thread,
  ``atexit`` hook or fork hook is ever created.
* **Fork-safe.** Threads do not survive ``fork()``, and a lock held by another
  thread at fork time stays held forever in the child. An
  ``os.register_at_fork(after_in_child=…)`` hook therefore gives the child
  fresh locks, an empty buffer (the parent still owns and sends those
  messages; the child would duplicate them), its own socket, and a flusher
  that restarts on the child's first metric.
"""

from __future__ import annotations

import atexit
import functools
import inspect
import logging
import os
import random as _random
import threading
import time
import weakref
from collections import deque
from collections.abc import Awaitable, Callable, Sequence
from dataclasses import dataclass
from types import TracebackType
from typing import Any, ParamSpec, TypeVar, cast

from ._config import Config
from ._format import format_line, format_number, sanitize_tag
from ._transport import Resolver, SocketFactory, UdpTransport

MAX_PENDING_PAYLOADS = 64
"""Full payloads allowed to wait for the flusher (~90 KiB at the default size)."""

_JOIN_TIMEOUT = 1.0
"""Seconds ``close()`` waits for the flusher thread; it is a daemon either way."""

_log = logging.getLogger("ozy")

P = ParamSpec("P")
R = TypeVar("R")

Tags = Sequence[str] | None
"""Call tags: ``["key:value", "bare"]``. A single string is treated as one tag."""


@dataclass(frozen=True, slots=True)
class Stats:
    """Counters since ``init()`` (or since ``fork()`` in a child process).

    UDP gives no delivery receipts, so these count what the SDK can observe:

    Attributes:
        sent: Messages (lines) in datagrams the kernel accepted. Accepted is
            not delivered: a datagram can still be lost in the network or by
            an agent that is not listening.
        packets: Datagrams the kernel accepted. ``sent / packets`` is the
            average number of messages coalesced into one datagram.
        dropped: Messages that were never handed to the kernel: invalid
            values (NaN, ±Inf, non-numbers), payloads evicted from a full
            queue, and payloads whose send failed. Sampled-out calls are
            intentional and not counted.
        errors: Failed operations: DNS failures, socket errors, and internal
            errors swallowed by the no-raise wrapper.
    """

    sent: int = 0
    packets: int = 0
    dropped: int = 0
    errors: int = 0


def _safe[F: Callable[..., Any]](method: F) -> F:
    """Wrap a public method so no ``Exception`` escapes into the caller.

    ``BaseException`` subclasses such as ``KeyboardInterrupt`` still pass:
    they are the user's intent, not our failure.
    """

    @functools.wraps(method)
    def wrapper(self: StatsdClient, *args: Any, **kwargs: Any) -> Any:
        try:
            return method(self, *args, **kwargs)
        except Exception:
            self._count_error("internal error in statsd." + method.__name__)
            return None

    return cast(F, wrapper)


class StatsdClient:
    """An extended StatsD client with buffering, a background flusher and a no-raise API.

    Most applications use the process-wide instance ``ozy.statsd``,
    configured by ``ozy.init()``. Constructing a separate client is for
    tests or for sending to a second agent; its dependencies (random source,
    clock, resolver, socket factory) are injectable for exactly that reason.

    Thread safety: every method may be called from any thread. The buffer is
    guarded by one lock, and sends are serialized by another so the explicit
    ``flush()`` and the background flusher never interleave on the socket.
    """

    def __init__(
        self,
        *,
        random: Callable[[], float] | None = None,
        clock: Callable[[], float] | None = None,
        resolver: Resolver | None = None,
        socket_factory: SocketFactory | None = None,
    ) -> None:
        """Create a disabled client; ``configure()`` (via ``init()``) enables it.

        Args:
            random: Source for sampling decisions, returning floats in
                [0, 1). A message with rate ``r`` is sent when
                ``random() < r``. Defaults to ``random.random``.
            clock: Monotonic seconds, for ``timed`` durations and the DNS
                cache. Defaults to ``time.perf_counter``.
            resolver: ``socket.getaddrinfo`` replacement for tests.
            socket_factory: ``socket.socket`` replacement for tests.
        """
        self._random = random or _random.random
        self._clock = clock or time.perf_counter
        self._resolver = resolver
        self._socket_factory = socket_factory

        self._config = Config()
        self._global_tags: tuple[str, ...] = ()
        self._transport: UdpTransport | None = None
        self._hooks_registered = False
        self._reset_runtime_state()

    # --- lifecycle ------------------------------------------------------------

    def _reset_runtime_state(self) -> None:
        # Everything that must be fresh after construction and after fork().
        self._lock = threading.Lock()
        self._send_lock = threading.Lock()
        self._wake = threading.Event()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._buffer: list[str] = []
        self._buffer_size = 0
        self._pending: deque[tuple[bytes, int]] = deque()
        self._sent = 0
        self._packets = 0
        self._dropped = 0
        self._errors = 0

    @property
    def enabled(self) -> bool:
        """Whether this client sends anything (an agent host is configured)."""
        return self._transport is not None

    def configure(self, config: Config) -> None:
        """Apply a configuration, replacing any previous one.

        A previously enabled client is closed first (its buffer is flushed to
        the old agent). With ``config.agent_host`` unset this leaves the
        client disabled and creates nothing. Unlike the metric methods this
        is not wrapped: a failure here is a programming error in the SDK.
        """
        self.close()
        self._reset_runtime_state()
        self._config = config
        self._global_tags = config.global_tags()
        if not config.enabled:
            return
        assert config.agent_host is not None
        self._transport = UdpTransport(
            config.agent_host,
            config.statsd_port,
            clock=self._clock,
            resolver=self._resolver,
            socket_factory=self._socket_factory,
        )
        self._register_process_hooks()

    def _register_process_hooks(self) -> None:
        # Once per client, and only once it is enabled: a disabled SDK must
        # leave no trace in the process. Both hooks hold a weak reference so
        # a discarded client (common in tests) can still be collected.
        if self._hooks_registered:
            return
        self._hooks_registered = True
        ref = weakref.ref(self)

        def at_exit() -> None:
            client = ref()
            if client is not None:
                client.close()

        def after_fork_in_child() -> None:
            client = ref()
            if client is not None:
                client._after_fork_in_child()

        atexit.register(at_exit)
        os.register_at_fork(after_in_child=after_fork_in_child)

    def _after_fork_in_child(self) -> None:
        # The parent's thread does not exist here, its locks may be held
        # forever, and its buffered messages are the parent's to send.
        transport = self._transport
        self._reset_runtime_state()
        if transport is not None:
            # The fd is a copy of the parent's; closing it here does not
            # affect the parent. A fresh socket is opened on the next send.
            transport.close()

    @_safe
    def close(self) -> None:
        """Flush, stop the background thread, close the socket and disable the client.

        Registered with ``atexit`` when the client is enabled, so buffered
        metrics are sent on normal interpreter exit. After ``close()`` every
        method is a no-op until ``init()``/``configure()`` is called again.
        """
        with self._lock:
            transport, self._transport = self._transport, None
            if transport is None:
                return
            # Disabling and draining under one lock hold means no message can
            # slip into the buffer after the final drain and be lost silently.
            batches = self._take_batches_locked(include_buffer=True)
        self._stop.set()
        self._wake.set()
        thread = self._thread
        if thread is not None and thread is not threading.current_thread():
            thread.join(_JOIN_TIMEOUT)
        self._thread = None
        self._send_batches(transport, batches)
        with self._send_lock:
            transport.close()

    # --- sending --------------------------------------------------------------

    def _count(self, *, sent: int = 0, packets: int = 0, dropped: int = 0, errors: int = 0) -> None:
        # `x += n` on an attribute is a read-modify-write that another thread
        # can interleave with, so counters are updated under the buffer lock.
        with self._lock:
            self._sent += sent
            self._packets += packets
            self._dropped += dropped
            self._errors += errors

    def _count_error(self, message: str, exc: BaseException | None = None) -> None:
        self._count(errors=1)
        if self._config.debug:
            _log.warning("ozy: %s", message, exc_info=exc)

    def _enqueue(self, line: str) -> None:
        data_size = len(line.encode("utf-8"))
        with self._lock:
            if self._transport is None:
                return
            # +1 for the "\n" separator between messages in a datagram.
            if self._buffer and self._buffer_size + 1 + data_size > self._config.max_payload:
                self._push_pending_locked()
            if self._buffer:
                self._buffer_size += 1
            self._buffer.append(line)
            self._buffer_size += data_size
            self._ensure_thread_locked()

    def _take_buffer_locked(self) -> tuple[bytes, int]:
        payload = "\n".join(self._buffer).encode("utf-8")
        count = len(self._buffer)
        self._buffer = []
        self._buffer_size = 0
        return payload, count

    def _push_pending_locked(self) -> None:
        payload, count = self._take_buffer_locked()
        if len(self._pending) >= MAX_PENDING_PAYLOADS:
            _, evicted = self._pending.popleft()
            self._dropped += evicted
        self._pending.append((payload, count))
        self._wake.set()

    def _ensure_thread_locked(self) -> None:
        if self._thread is not None and self._thread.is_alive():
            return
        # A weak reference, not the bound method: a running Thread holds its
        # target for as long as it lives, so `target=self._run_flusher` would
        # keep a discarded client — and its socket — alive until the process
        # exits, which is exactly what the weakrefs in _register_process_hooks
        # exist to avoid.
        self._thread = threading.Thread(
            target=_run_flusher,
            args=(weakref.ref(self), self._stop, self._wake, self._config.flush_interval),
            name="ozy-statsd-flusher",
            daemon=True,  # never keeps the interpreter alive; atexit flushes
        )
        self._thread.start()

    def _take_batches_locked(self, *, include_buffer: bool) -> list[tuple[bytes, int]]:
        batches = list(self._pending)
        self._pending.clear()
        if include_buffer and self._buffer:
            # Straight into the batch, not via the bounded queue: draining
            # must never evict.
            batches.append(self._take_buffer_locked())
        return batches

    def _flush(self, *, include_buffer: bool) -> None:
        with self._lock:
            transport = self._transport
            batches = self._take_batches_locked(include_buffer=include_buffer)
        if transport is not None:
            self._send_batches(transport, batches)

    def _send_batches(self, transport: UdpTransport, batches: list[tuple[bytes, int]]) -> None:
        # I/O happens outside the buffer lock so callers keep enqueueing
        # while a datagram is in flight.
        if not batches:
            return
        with self._send_lock:
            for payload, count in batches:
                try:
                    transport.send(payload)
                except Exception as exc:
                    # Deliberately broader than OSError. Whatever goes wrong,
                    # the invariants callers rely on are the same: these
                    # messages are counted dropped, and the remaining batches
                    # still get their turn. An escape here would abandon up to
                    # 63 more batches, leave `dropped` understating the loss,
                    # and — from close() — skip closing the socket.
                    self._count(dropped=count)
                    self._count_error(f"send to {self._config.agent_host} failed: {exc}")
                    continue
                self._count(sent=count, packets=1)
                if self._config.debug:
                    _log.debug("ozy: sent %d bytes: %r", len(payload), payload)

    # --- public API -----------------------------------------------------------

    def _submit(
        self,
        name: str,
        value: str | None,
        metric_type: str,
        tags: Tags,
        sample_rate: float,
    ) -> None:
        if self._transport is None:
            return
        if value is None:
            self._count(dropped=1)  # NaN, ±Inf or not a number: never sent
            return
        if sample_rate < 1 and not self._random() < sample_rate:
            return
        if tags:
            call_tags = (
                [sanitize_tag(tags)] if isinstance(tags, str) else [sanitize_tag(t) for t in tags]
            )
            all_tags: Sequence[str] = call_tags + list(self._global_tags)
        else:
            all_tags = self._global_tags
        self._enqueue(format_line(name, value, metric_type, sample_rate, all_tags))

    @staticmethod
    def _number(value: object, *, negate: bool = False) -> str | None:
        try:
            return format_number(-value if negate else value)  # type: ignore[operator]
        except (TypeError, ValueError, OverflowError):
            return None

    @_safe
    def increment(
        self, name: str, value: float = 1, tags: Tags = None, sample_rate: float = 1.0
    ) -> None:
        """Count an event (type ``c``).

        The agent sums counts per 10 s bucket and scales each by
        ``1/sample_rate``, so sampled counts still estimate the true total.

        Args:
            name: Metric name, e.g. ``"http.request.count"``.
            value: Amount to add; may be fractional or negative.
            tags: ``"key:value"`` tags for this call, sent before the global tags.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:  # fast path for the disabled SDK
            return
        self._submit(name, self._number(value), "c", tags, sample_rate)

    @_safe
    def decrement(
        self, name: str, value: float = 1, tags: Tags = None, sample_rate: float = 1.0
    ) -> None:
        """Count down: exactly ``increment(name, -value, …)``.

        Args:
            name: Metric name.
            value: Amount to subtract.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, self._number(value, negate=True), "c", tags, sample_rate)

    @_safe
    def gauge(self, name: str, value: float, tags: Tags = None, sample_rate: float = 1.0) -> None:
        """Record the current value of something (type ``g``); the agent keeps the last one.

        Args:
            name: Metric name, e.g. ``"queue.depth"``.
            value: The current value.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, self._number(value), "g", tags, sample_rate)

    @_safe
    def histogram(
        self, name: str, value: float, tags: Tags = None, sample_rate: float = 1.0
    ) -> None:
        """Record a sample of a distribution aggregated by the agent (type ``h``).

        The agent emits avg/min/max/median/p95 gauges and a count per host.
        Prefer :meth:`distribution` when percentiles must be combined across
        hosts: averaging per-host p95s is not a p95.

        Args:
            name: Metric name.
            value: The sample.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, self._number(value), "h", tags, sample_rate)

    @_safe
    def distribution(
        self, name: str, value: float, tags: Tags = None, sample_rate: float = 1.0
    ) -> None:
        """Record a sample for a globally mergeable distribution (type ``d``).

        From M2 the agent folds these into a DDSketch, which can be merged
        across hosts and still answer any percentile within 1% relative error.
        In M1 the agent treats ``d`` like ``h``.

        Args:
            name: Metric name, e.g. ``"http.request.duration"``.
            value: The sample.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, self._number(value), "d", tags, sample_rate)

    @_safe
    def timing(self, name: str, value: float, tags: Tags = None, sample_rate: float = 1.0) -> None:
        """Record a duration in milliseconds (type ``ms``, aggregated like ``h``).

        Args:
            name: Metric name, e.g. ``"job.duration"``.
            value: Duration in milliseconds.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, self._number(value), "ms", tags, sample_rate)

    @_safe
    def set(self, name: str, value: object, tags: Tags = None, sample_rate: float = 1.0) -> None:
        """Count distinct values (type ``s``); the agent reports the unique count per bucket.

        Args:
            name: Metric name, e.g. ``"users.unique"``.
            value: The member; sent as ``str(value)`` with ``|``, ``,`` and
                newline replaced by ``_``.
            tags: Tags for this call.
            sample_rate: Fraction of calls actually sent, in (0, 1].
        """
        if self._transport is None:
            return
        self._submit(name, sanitize_tag(str(value)), "s", tags, sample_rate)

    def timed(self, name: str, tags: Tags = None, sample_rate: float = 1.0) -> Timed:
        """Time a block or function and record it with :meth:`timing` (milliseconds).

        Usable three ways::

            with statsd.timed("job.duration"):
                run_job()

            @statsd.timed("job.duration")
            def run_job(): ...

            @statsd.timed("fetch.duration")
            async def fetch(): ...

        The duration is recorded even when the timed code raises, and the
        exception propagates unchanged — the SDK never swallows or wraps
        the app's errors. Creating the timer never raises.

        Args:
            name: Metric name for the ``ms`` timing.
            tags: Tags for this call.
            sample_rate: Fraction of timings actually sent, in (0, 1].

        Returns:
            A :class:`Timed` context manager / decorator.
        """
        return Timed(self, name, tags, sample_rate)

    @_safe
    def flush(self) -> None:
        """Send everything buffered now, on the calling thread.

        Normally unnecessary (the background flusher runs every 100 ms);
        useful before a process exits without ``atexit`` (``os._exit``), at
        the end of a short-lived job, or in tests.
        """
        if self._transport is None:
            return
        self._flush(include_buffer=True)

    def stats(self) -> Stats:
        """Return a snapshot of the sent / packets / dropped / errors counters. Never raises.

        Same shape as the Node SDK's ``stats()``.
        """
        return Stats(
            sent=self._sent, packets=self._packets, dropped=self._dropped, errors=self._errors
        )


class Timed:
    """Context manager and decorator returned by :meth:`StatsdClient.timed`.

    As a decorator each call is timed independently, so a decorated function
    may run concurrently and recursively. As a context manager one ``Timed``
    object tracks one active block at a time; nested ``with`` on the *same*
    object is supported (starts are kept on a stack) but sharing one object
    across threads is not — call ``statsd.timed(...)`` per block instead.
    """

    __slots__ = ("_client", "_name", "_sample_rate", "_starts", "_tags")

    def __init__(self, client: StatsdClient, name: str, tags: Tags, sample_rate: float) -> None:
        """Capture what to record; the clock starts on ``__enter__`` or each call."""
        self._client = client
        self._name = name
        self._tags = tags
        self._sample_rate = sample_rate
        self._starts: list[float] = []

    def _record(self, start: float) -> None:
        client = self._client
        if not client.enabled:
            return
        try:
            elapsed_ms = (client._clock() - start) * 1000.0
        except Exception:
            client._count_error("timed: clock failed")
            return
        client.timing(self._name, elapsed_ms, self._tags, self._sample_rate)

    def _now(self) -> float:
        try:
            return self._client._clock()
        except Exception:
            self._client._count_error("timed: clock failed")
            return 0.0

    def __enter__(self) -> Timed:
        """Start the clock."""
        self._starts.append(self._now())
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        """Record the elapsed time; any exception from the block propagates."""
        start = self._starts.pop() if self._starts else self._now()
        self._record(start)

    async def __aenter__(self) -> Timed:
        """Start the clock (``async with`` form)."""
        return self.__enter__()

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        """Record the elapsed time; any exception from the block propagates."""
        self.__exit__(exc_type, exc, tb)

    def __call__(self, func: Callable[P, R]) -> Callable[P, R]:
        """Decorate ``func`` (sync or ``async def``) so every call is timed."""
        if inspect.iscoroutinefunction(func):
            async_func = cast(Callable[P, Awaitable[Any]], func)

            @functools.wraps(func)
            async def async_wrapper(*args: P.args, **kwargs: P.kwargs) -> Any:
                start = self._now()
                try:
                    return await async_func(*args, **kwargs)
                finally:
                    self._record(start)

            return cast(Callable[P, R], async_wrapper)

        @functools.wraps(func)
        def wrapper(*args: P.args, **kwargs: P.kwargs) -> R:
            start = self._now()
            try:
                return func(*args, **kwargs)
            finally:
                self._record(start)

        return wrapper


def _run_flusher(
    ref: weakref.ref[StatsdClient],
    stop: threading.Event,
    wake: threading.Event,
    interval: float,
) -> None:
    # Everything this loop needs is passed in rather than read back off the
    # client, so a flusher left over from before a configure() can never act
    # on the new state. The client itself is reached through a weak reference
    # and released before each wait, so the thread never keeps it alive.
    while not stop.is_set():
        woken = wake.wait(interval)
        if stop.is_set():
            return
        client = ref()
        if client is None:
            return  # the client was collected; there is nothing left to flush
        try:
            if woken:
                wake.clear()
                # A buffer filled up: send the full payloads only, and let the
                # fresh buffer keep filling.
                client._flush(include_buffer=False)
            else:
                client._flush(include_buffer=True)
        except Exception as exc:  # pragma: no cover - _flush does not raise
            client._count_error("flusher failed", exc)
        del client
