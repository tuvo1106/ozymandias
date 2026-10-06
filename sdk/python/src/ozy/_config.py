"""Configuration: ``init()`` arguments layered over ``OZY_*`` environment variables.

Precedence is *argument > environment > default*. Environment variables let
an operator point an unmodified app at an agent (twelve-factor style);
arguments let the app pin values it owns, like its service name.

The single most important setting is the agent host. When neither the
``agent_host`` argument nor ``OZY_AGENT_HOST`` is set, the SDK is
**disabled**: every call is a no-op and no socket, thread or exit hook is
created. That is what makes it safe to leave instrumentation in code that
runs in tests, CI and on machines with no agent.

Parsing never raises. A malformed ``OZY_STATSD_PORT`` falls back to the
default, because an observability library must not be the reason an app
fails to boot.
"""

from __future__ import annotations

import os
from collections.abc import Mapping, Sequence
from dataclasses import dataclass

from ._format import sanitize_tag

DEFAULT_STATSD_PORT = 8125
"""The extended StatsD convention; the agent listens here by default."""

DEFAULT_TRACE_PORT = 8126
"""Where the agent's trace receiver listens (``POST /v1/traces``, TCP)."""

DEFAULT_TRACE_SAMPLE_RATE = 1.0
"""Keep every trace until the agent says otherwise (``rate_by_service``)."""

DEFAULT_MAX_PAYLOAD = 1432
"""Bytes per datagram: one Ethernet MTU (1500) minus IPv4/UDP headers and slack.

Staying under the MTU avoids IP fragmentation, where losing any one fragment
loses the whole datagram.
"""

DEFAULT_FLUSH_INTERVAL = 0.1
"""Seconds a partially filled buffer may wait before it is sent anyway."""

_TRUTHY = frozenset({"1", "true", "yes", "on"})
_FALSY = frozenset({"0", "false", "no", "off"})


@dataclass(frozen=True, slots=True)
class Config:
    """Resolved SDK settings. Immutable: reconfiguring means calling ``init()`` again.

    Attributes:
        agent_host: Agent hostname or IP; ``None`` disables the SDK.
        statsd_port: Agent UDP port for extended StatsD.
        service: Value of the global ``service:`` tag, if any.
        env: Value of the global ``env:`` tag, if any.
        version: Value of the global ``version:`` tag, if any.
        tags: Extra global tags appended to every message, after call tags.
        debug: Log send failures and payloads to the ``ozy`` logger.
        max_payload: Maximum datagram size in bytes.
        flush_interval: Seconds between background flushes.
        trace_enabled: Whether tracing is on when an agent host is configured.
        trace_port: Agent TCP port for ``POST /v1/traces``.
        trace_sample_rate: Head-sampling rate in [0, 1] until the agent overrides it.
    """

    agent_host: str | None = None
    statsd_port: int = DEFAULT_STATSD_PORT
    service: str | None = None
    env: str | None = None
    version: str | None = None
    tags: tuple[str, ...] = ()
    debug: bool = False
    max_payload: int = DEFAULT_MAX_PAYLOAD
    flush_interval: float = DEFAULT_FLUSH_INTERVAL
    trace_enabled: bool = True
    trace_port: int = DEFAULT_TRACE_PORT
    trace_sample_rate: float = DEFAULT_TRACE_SAMPLE_RATE

    @property
    def enabled(self) -> bool:
        """Whether metrics are sent at all (an agent host is configured)."""
        return bool(self.agent_host)

    @property
    def traces_enabled(self) -> bool:
        """Whether spans are recorded: an agent host is configured and tracing not switched off."""
        return self.enabled and self.trace_enabled

    def global_tags(self) -> tuple[str, ...]:
        """Tags appended to every message, in wire order, already sanitized.

        Order is fixed by the wire protocol: the ``init``/``OZY_TAGS``
        tags, then ``service:``, ``env:`` and ``version:`` for whichever are
        set. Computing this once per ``init()`` keeps it off the hot path.
        """
        out = [sanitize_tag(t) for t in self.tags if t]
        for key, value in (("service", self.service), ("env", self.env), ("version", self.version)):
            if value:
                out.append(sanitize_tag(f"{key}:{value}"))
        return tuple(out)


def _env_str(env: Mapping[str, str], key: str) -> str | None:
    value = env.get(key, "").strip()
    return value or None


def _parse_port(raw: str | None, default: int = DEFAULT_STATSD_PORT) -> int:
    if raw is None:
        return default
    try:
        port = int(raw)
    except ValueError:
        return default
    return port if 0 < port < 65536 else default


def _parse_rate(raw: str | float | None) -> float:
    """A sample rate clamped to [0, 1]; anything unparseable (or NaN) is the default."""
    if raw is None:
        return DEFAULT_TRACE_SAMPLE_RATE
    try:
        rate = float(raw)
    except (TypeError, ValueError):
        return DEFAULT_TRACE_SAMPLE_RATE
    if rate != rate:  # NaN
        return DEFAULT_TRACE_SAMPLE_RATE
    return min(1.0, max(0.0, rate))


def resolve_config(
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
    environ: Mapping[str, str] | None = None,
) -> Config:
    """Merge explicit arguments over ``OZY_*`` environment variables.

    ``None`` means "not given" and falls through to the environment; any
    other value wins, including ``agent_host=""``, which explicitly disables
    the SDK even when ``OZY_AGENT_HOST`` is set.

    Args:
        service: Global ``service:`` tag (``OZY_SERVICE``).
        env: Global ``env:`` tag (``OZY_ENV``).
        version: Global ``version:`` tag (``OZY_VERSION``).
        tags: Extra global tags (``OZY_TAGS``, comma separated).
        agent_host: Agent host (``OZY_AGENT_HOST``); unset disables.
        statsd_port: Agent UDP port (``OZY_STATSD_PORT``, default 8125).
        debug: Log failures and payloads (``OZY_DEBUG``: 1/true/yes/on).
        max_payload: Datagram size limit in bytes (default 1432).
        flush_interval: Background flush period in seconds (default 0.1).
        trace_enabled: Record spans when an agent host is set (``OZY_TRACE_ENABLED``,
            default true; ``0``/``false``/``no``/``off`` disables).
        trace_port: Agent TCP port for traces (``OZY_TRACE_PORT``, default 8126).
        trace_sample_rate: Head-sampling rate, clamped to [0, 1]
            (``OZY_TRACE_SAMPLE_RATE``, default 1.0).
        environ: Environment to read; defaults to ``os.environ`` (tests pass
            a dict instead of mutating the process environment).

    Returns:
        The resolved, immutable configuration.
    """
    environ = os.environ if environ is None else environ

    if tags is None:
        raw_tags = environ.get("OZY_TAGS", "")
        resolved_tags = tuple(t.strip() for t in raw_tags.split(",") if t.strip())
    elif isinstance(tags, str):
        # A lone string is almost certainly one tag, not a sequence of
        # one-character tags.
        resolved_tags = (tags,)
    else:
        resolved_tags = tuple(str(t) for t in tags)

    if debug is None:
        debug = environ.get("OZY_DEBUG", "").strip().lower() in _TRUTHY

    if trace_enabled is None:
        trace_enabled = environ.get("OZY_TRACE_ENABLED", "").strip().lower() not in _FALSY

    return Config(
        agent_host=(agent_host if agent_host is not None else _env_str(environ, "OZY_AGENT_HOST"))
        or None,
        statsd_port=(
            statsd_port
            if statsd_port is not None
            else _parse_port(_env_str(environ, "OZY_STATSD_PORT"))
        ),
        service=service if service is not None else _env_str(environ, "OZY_SERVICE"),
        env=env if env is not None else _env_str(environ, "OZY_ENV"),
        version=version if version is not None else _env_str(environ, "OZY_VERSION"),
        tags=resolved_tags,
        debug=debug,
        max_payload=max_payload
        if max_payload is not None and max_payload > 0
        else DEFAULT_MAX_PAYLOAD,
        flush_interval=(
            flush_interval
            if flush_interval is not None and flush_interval > 0
            else DEFAULT_FLUSH_INTERVAL
        ),
        trace_enabled=trace_enabled,
        trace_port=(
            trace_port
            if trace_port is not None and 0 < trace_port < 65536
            else _parse_port(_env_str(environ, "OZY_TRACE_PORT"), DEFAULT_TRACE_PORT)
        ),
        trace_sample_rate=(
            _parse_rate(trace_sample_rate)
            if trace_sample_rate is not None
            else _parse_rate(_env_str(environ, "OZY_TRACE_SAMPLE_RATE"))
        ),
    )
