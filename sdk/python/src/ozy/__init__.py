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

from ._config import Config, resolve_config
from ._statsd import Stats, StatsdClient, Timed

__version__ = "0.1.0"

__all__ = ["Config", "Stats", "StatsdClient", "Timed", "__version__", "init", "statsd"]

statsd: StatsdClient = StatsdClient()
"""The process-wide statsd client, disabled until :func:`init` enables it.

It exists from import time so ``from ozy import statsd`` works anywhere,
in any import order; ``init()`` reconfigures this same object rather than
replacing it, so references taken before ``init()`` stay valid.
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
        )
        statsd.configure(config)
    except Exception:
        logging.getLogger("ozy").warning("ozy: init failed; SDK disabled", exc_info=True)
        with contextlib.suppress(Exception):
            statsd.configure(Config())
