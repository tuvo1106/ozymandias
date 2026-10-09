"""ozy: send metrics from any Python app to an ozymandias agent.

Mental model
------------
The SDK is a thin, fire-and-forget producer of **extended StatsD datagrams**
(``docs/wire-protocol.md`` §A). All aggregation — summing counters, computing
percentiles, attaching the host tag — happens in the agent, so a call like
``statsd.increment("page.views")`` only formats one short line and appends it
to an in-memory buffer. A background thread ships the buffer over UDP every
100 ms. The app never waits on the network and never sees an exception from
this package.

Usage::

    import ozy
    from ozy import statsd

    ozy.init(service="shop", env="dev", version="1.2.0")
    statsd.increment("checkout.completed", tags=["payment:card"])

    with statsd.timed("checkout.duration"):
        ...

Traces use the same ``init()`` and the same disabled-means-inert rule::

    from ozy import tracer

    with tracer.trace("judge.run", resource="python", type="worker") as span:
        span.set_tag("problem.id", pid)

    ozy.init(service="shop", integrations=["sqlalchemy", "redis", "httpx"])

Without ``OZY_AGENT_HOST`` (or ``init(agent_host=...)``) the SDK is
disabled and every call is a no-op, so instrumentation can stay in code that
runs in tests or on machines with no agent.

The wire protocol, not this package, is ozymandias's public interface: any
extended StatsD client can talk to the agent. This SDK exists to get the details
(formatting, buffering, sampling, fork safety) right once.
"""

from __future__ import annotations

import contextlib
import logging
from collections.abc import Sequence
from typing import TYPE_CHECKING

from ._config import Config, resolve_config
from ._statsd import Stats, StatsdClient, Timed
from ._tracer import Span, Tracer, TracerStats
from ._tracing import Context, normalize_path

if TYPE_CHECKING:
    from .integrations import Integration

__version__ = "0.2.0"

__all__ = [
    "Config",
    "Context",
    "Span",
    "Stats",
    "StatsdClient",
    "Timed",
    "Tracer",
    "TracerStats",
    "__version__",
    "init",
    "normalize_path",
    "statsd",
    "tracer",
]

statsd: StatsdClient = StatsdClient()
"""The process-wide statsd client, disabled until :func:`init` enables it.

It exists from import time so ``from ozy import statsd`` works anywhere,
in any import order; ``init()`` reconfigures this same object rather than
replacing it, so references taken before ``init()`` stay valid.
"""

tracer: Tracer = Tracer()
"""The process-wide tracer, disabled until :func:`init` enables it.

Like :data:`statsd` it exists from import time and ``init()`` reconfigures this same
object. Disabled, ``tracer.trace(...)`` returns a no-op span and your code still runs.
"""


def init(
    *,
    service: str | None = None,
    env: str | None = None,
    version: str | None = None,
    tags: Sequence[str] | None = None,
    agent_host: str | None = None,
    statsd_port: int | None = None,
    debug: bool | None = None,
    max_payload: int | None = None,
    flush_interval: float | None = None,
    trace_enabled: bool | None = None,
    trace_port: int | None = None,
    trace_sample_rate: float | None = None,
    integrations: Sequence[str | Integration] | None = None,
) -> None:
    """Configure the SDK from arguments layered over ``OZY_*`` environment variables.

    Call once at startup. Arguments override the environment; an argument
    left as ``None`` falls back to its variable, then to the default. Calling
    ``init()`` again flushes and closes the previous configuration first.

    Nothing is created here: the socket opens on the first send and the
    flusher thread starts on the first metric. When enabled, ``init()``
    registers ``statsd.close`` with ``atexit`` (so buffered metrics are sent
    on exit) and a fork hook (so child processes get their own flusher).
    When disabled it registers nothing.

    Never raises: a failure is logged to the ``ozy`` logger and leaves
    the SDK disabled, because instrumentation must not stop an app booting.

    Args:
        service: Global ``service:`` tag. Env: ``OZY_SERVICE``.
        env: Global ``env:`` tag. Env: ``OZY_ENV``.
        version: Global ``version:`` tag. Env: ``OZY_VERSION``.
        tags: Extra tags on every metric. Env: ``OZY_TAGS`` (comma list).
        agent_host: Agent host; unset disables the SDK. Env:
            ``OZY_AGENT_HOST``. ``""`` disables even if the env var is set.
        statsd_port: Agent UDP port. Env: ``OZY_STATSD_PORT``. Default 8125.
        debug: Log send failures (warning) and payloads (debug) to the
            ``ozy`` logger. Env: ``OZY_DEBUG`` (``1``/``true``/``yes``/``on``).
        max_payload: Datagram size limit in bytes. Default 1432 (one MTU).
            Raise it only for loopback/jumbo-frame networks; the agent reads
            at most 8192.
        flush_interval: Seconds between background flushes. Default 0.1.
        trace_enabled: Record spans when an agent host is set. Env:
            ``OZY_TRACE_ENABLED``. Default true.
        trace_port: Agent TCP port for traces. Env: ``OZY_TRACE_PORT``. Default 8126.
        trace_sample_rate: Head-sampling rate in [0, 1] until the agent's
            ``rate_by_service`` overrides it. Env: ``OZY_TRACE_SAMPLE_RATE``. Default 1.
        integrations: Names (``"sqlalchemy"``, ``"redis"``, ``"httpx"``, ``"arq"``,
            ``"logging"``, ``"asgi"``) or :class:`~ozy.integrations.Integration`
            objects to patch, only when tracing is enabled. Each one whose library is
            missing is skipped.
    """
    try:
        config = resolve_config(
            service=service,
            env=env,
            version=version,
            tags=tags,
            agent_host=agent_host,
            statsd_port=statsd_port,
            debug=debug,
            max_payload=max_payload,
            flush_interval=flush_interval,
            trace_enabled=trace_enabled,
            trace_port=trace_port,
            trace_sample_rate=trace_sample_rate,
        )
        statsd.configure(config)
        tracer.configure(config)
        if integrations and config.traces_enabled:
            from .integrations import patch

            patch(integrations)
    except Exception:
        logging.getLogger("ozy").warning("ozy: init failed; SDK disabled", exc_info=True)
        with contextlib.suppress(Exception):
            statsd.configure(Config())
            tracer.configure(Config())
