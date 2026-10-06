"""The trace writer: a bounded queue of finished chunks, shipped by one background thread.

Data path for one trace::

    caller thread (span.finish)          flusher thread (daemon, lazy)
    ───────────────────────────          ─────────────────────────────
    root finishes ─► submit(chunk)       every 1 s, or woken at 100 queued chunks:
        lock ─► append ─► unlock             take everything ─► JSON ─► POST /v1/traces
        (wake if 100 queued)                 read rate_by_service from the 200 response

Same contract as the statsd client, adapted to HTTP:

* **The caller never waits on the network.** ``submit`` is a lock and a list append.
* **Bounded memory.** At most ``MAX_QUEUED_CHUNKS`` chunks wait; past that the
  *oldest* is dropped and counted (recent traces are worth more, and an agent that is
  down must not grow the host's heap).
* **Best-effort.** A 2 s timeout, no retries: a trace that did not arrive is gone, and
  ``429`` ("agent is busy decoding") is answered by dropping, not by trying again.
* **Fork-safe.** Threads do not survive ``fork()`` and a lock held by another thread at
  fork time stays held forever in the child; the after-fork hook gives the child fresh
  locks, an empty queue (the parent still owns and sends those chunks) and no thread,
  which restarts on the child's first finished trace.
* **Flush on exit** with a 1 s budget: the thread drains once more and is joined for at
  most that long, so a dead agent delays interpreter exit by at most a second.

Rejected alternative: a connection kept open between flushes. One request per second
does not need it, and a stale keep-alive socket is a failure mode (the first request
after an agent restart fails) that a fresh connection each time simply does not have.
"""

from __future__ import annotations

import http.client
import json
import logging
import os
import threading
import time
from collections import deque
from collections.abc import Callable, Mapping
from typing import Any

MAX_QUEUED_CHUNKS = 1000
FLUSH_INTERVAL = 1.0
FLUSH_CHUNKS = 100
REQUEST_TIMEOUT = 2.0
EXIT_BUDGET = 1.0
MAX_BODY_BYTES = 8 * 1024 * 1024
"""Under the agent's 10 MiB body limit, with room for the envelope."""
MAX_CHUNKS_PER_BODY = 1000
"""The agent refuses a body with more chunks than this (wire-protocol.md §B)."""

_log = logging.getLogger("ozy")

Chunk = list[dict[str, Any]]


class TraceWriter:
    """Queues finished chunks and posts them to the agent from a background thread.

    Args:
        host: Agent hostname or IP.
        port: Agent trace port.
        tracer_info: The ``tracer`` object of every body (lang, versions).
        on_rates: Called with the ``rate_by_service`` object of a successful response.
        flush_interval: Seconds a queued chunk may wait.
        flush_chunks: Queue length that wakes the flusher early.
        max_queue: Chunks held before the oldest is dropped.
        timeout: Seconds for connecting, sending and reading one request.
    """

    def __init__(
        self,
        host: str,
        port: int,
        *,
        tracer_info: Mapping[str, str],
        on_rates: Callable[[Mapping[str, Any]], None] | None = None,
        flush_interval: float = FLUSH_INTERVAL,
        flush_chunks: int = FLUSH_CHUNKS,
        max_queue: int = MAX_QUEUED_CHUNKS,
        timeout: float = REQUEST_TIMEOUT,
    ) -> None:
        """Create an idle writer: no thread and no socket until the first chunk."""
        self._host = host
        self._port = port
        self._tracer_info = dict(tracer_info)
        self._on_rates = on_rates
        self._interval = flush_interval
        self._flush_chunks = flush_chunks
        self._max_queue = max_queue
        self._timeout = timeout
        self._pid = os.getpid()
        self._reset_runtime_state()
        self.chunks_sent = 0
        self.chunks_dropped = 0
        self.send_errors = 0
        self._closed = False

    def _reset_runtime_state(self) -> None:
        self._lock = threading.Lock()
        self._send_lock = threading.Lock()
        self._queue: deque[Chunk] = deque()
        self._thread: threading.Thread | None = None
        self._stop = threading.Event()
        self._wake = threading.Event()

    # -- producer side (any thread) ------------------------------------------------

    def submit(self, chunk: Chunk) -> None:
        """Queue one chunk; never blocks on the network and never raises."""
        try:
            if self._closed or not chunk:
                return
            with self._lock:
                if len(self._queue) >= self._max_queue:
                    self._queue.popleft()
                    self.chunks_dropped += 1
                self._queue.append(chunk)
                full = len(self._queue) >= self._flush_chunks
                self._ensure_thread_locked()
            if full:
                self._wake.set()
        except Exception:
            _log.debug("ozy: queueing a trace chunk failed", exc_info=True)

    def _ensure_thread_locked(self) -> None:
        thread = self._thread
        if thread is not None and thread.is_alive():
            return
        self._thread = threading.Thread(
            target=self._run,
            args=(self._stop, self._wake),
            name="ozy-trace-flusher",
            daemon=True,  # never keeps the interpreter alive; close() drains on exit
        )
        self._thread.start()

    @property
    def queued(self) -> int:
        """Chunks waiting to be sent."""
        return len(self._queue)

    # -- flusher side --------------------------------------------------------------

    def _run(self, stop: threading.Event, wake: threading.Event) -> None:
        # The events are arguments, not attributes read each loop, so a thread left over
        # from before a fork or a close() can never wait on (or act for) newer state.
        while not stop.is_set():
            woken = wake.wait(self._interval)
            if woken:
                wake.clear()
            self._flush(self._timeout)
        # One last drain after stop, within the exit budget.
        self._flush(min(self._timeout, EXIT_BUDGET))

    def flush(self) -> None:
        """Send everything queued now, on the calling thread. Never raises."""
        self._flush(self._timeout)

    def _flush(self, timeout: float) -> None:
        deadline = time.monotonic() + max(timeout, 0.0) * 2
        try:
            with self._send_lock:
                with self._lock:
                    chunks = list(self._queue)
                    self._queue.clear()
                # Encoding happens outside the queue lock: it is the expensive part and
                # a caller finishing a span must never wait behind it.
                batch: list[bytes] = []
                size = 0
                for chunk in chunks:
                    encoded = self._encode(chunk)
                    if encoded is None:
                        continue
                    if batch and (
                        size + len(encoded) > MAX_BODY_BYTES or len(batch) >= MAX_CHUNKS_PER_BODY
                    ):
                        self._post_within(batch, timeout, deadline)
                        batch, size = [], 0
                    batch.append(encoded)
                    size += len(encoded)
                if batch:
                    self._post_within(batch, timeout, deadline)
        except Exception:
            self.send_errors += 1
            _log.debug("ozy: flushing traces failed", exc_info=True)

    def _post_within(self, batch: list[bytes], timeout: float, deadline: float) -> None:
        if time.monotonic() > deadline:
            self.chunks_dropped += len(batch)  # out of budget: do not stack timeouts
            return
        self._post(batch, timeout)

    def _encode(self, chunk: Chunk) -> bytes | None:
        try:
            data = json.dumps(chunk, separators=(",", ":"), allow_nan=False).encode("ascii")
        except (TypeError, ValueError):
            self.chunks_dropped += 1
            return None
        if len(data) > MAX_BODY_BYTES:
            self.chunks_dropped += 1  # one chunk the agent could never accept
            return None
        return data

    def _post(self, batch: list[bytes], timeout: float) -> None:
        body = (
            b'{"tracer":'
            + json.dumps(self._tracer_info).encode("ascii")
            + b',"traces":['
            + b",".join(batch)
            + b"]}"
        )
        conn = http.client.HTTPConnection(self._host, self._port, timeout=timeout)
        try:
            conn.request("POST", "/v1/traces", body, {"Content-Type": "application/json"})
            response = conn.getresponse()
            payload = response.read()
            if response.status == 200:
                self.chunks_sent += len(batch)
                self._read_rates(payload)
            else:
                # 429 means the agent is already decoding 8 bodies: drop and move on.
                self.chunks_dropped += len(batch)
                self.send_errors += 1
        except (OSError, http.client.HTTPException):
            self.chunks_dropped += len(batch)
            self.send_errors += 1
        finally:
            conn.close()

    def _read_rates(self, payload: bytes) -> None:
        if self._on_rates is None:
            return
        try:
            rates = json.loads(payload).get("rate_by_service")
            if isinstance(rates, dict):
                self._on_rates(rates)
        except (ValueError, AttributeError):
            pass  # a body we cannot read costs us the new rates, not the trace

    # -- lifecycle -----------------------------------------------------------------

    def after_fork_in_child(self) -> None:
        """Give a forked child its own locks, an empty queue and no thread.

        The parent's queued chunks are the parent's to send; the child would only
        duplicate them.
        """
        self._pid = os.getpid()
        self._reset_runtime_state()

    def close(self, budget: float = EXIT_BUDGET) -> None:
        """Drain the queue and stop the thread, waiting at most ``budget`` seconds."""
        self._closed = True
        self._stop.set()
        self._wake.set()
        thread = self._thread
        deadline = time.monotonic() + budget
        if thread is not None and thread is not threading.current_thread():
            thread.join(budget)
        elif self._queue:
            self._flush(max(0.05, min(self._timeout, deadline - time.monotonic())))
        self._thread = None
