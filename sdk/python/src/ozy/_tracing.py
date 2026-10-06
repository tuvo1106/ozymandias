"""Pure helpers shared by the tracer and its integrations: ids, sampling, paths, propagation.

Everything here is a pure function of its arguments (no clock, no state), which is
what lets the shared vector files in ``pkg/wire/testdata/traces/`` pin it from all
three languages. Where a rule has a Go twin (``pkg/wire/traces.go``) the twin is named
in the docstring; changing one without the other makes services disagree about a
trace, which nothing at runtime would reveal.
"""

from __future__ import annotations

import re
from collections.abc import Mapping
from dataclasses import dataclass
from typing import Any

__all__ = [
    "HEADER_PARENT_ID",
    "HEADER_PRIORITY",
    "HEADER_TRACE_ID",
    "Context",
    "normalize_path",
    "parse_propagation",
    "sample_keep",
]

HEADER_TRACE_ID = "x-ozy-trace-id"
HEADER_PARENT_ID = "x-ozy-parent-id"
HEADER_PRIORITY = "x-ozy-sampling-priority"

PRIORITY_USER_DROP = -1
PRIORITY_AUTO_DROP = 0
PRIORITY_AUTO_KEEP = 1
PRIORITY_USER_KEEP = 2

_MASK64 = (1 << 64) - 1
SAMPLING_MULTIPLIER = 1111111111111111111
"""Knuth's multiplicative hash constant; the decision depends on the trace id alone."""

MAX_PATH_SEGMENTS = 8

_HEX = re.compile(r"[0-9a-fA-F]+")
_DIGITS = re.compile(r"[0-9]+")
_HEX12 = re.compile(r"[0-9a-fA-F]{12,}")
_UUID = re.compile(r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}")
_NANO = re.compile(r"[A-Za-z0-9_-]{16,}")
_HAS_DIGIT = re.compile(r"[0-9]")


@dataclass(frozen=True, slots=True)
class Context:
    """The part of a span another span needs in order to become its child.

    This is what crosses a process boundary (headers, job kwargs): three values,
    no span object, so it can be compared (``extract(inject(ctx)) == ctx``) and
    cannot carry anything the receiver has to trust beyond its shape.

    Attributes:
        trace_id: 32 lowercase hex characters.
        span_id: 16 lowercase hex characters: the span that becomes the parent.
        sampling_priority: -1 user drop, 0 auto drop, 1 auto keep, 2 user keep.
    """

    trace_id: str
    span_id: str
    sampling_priority: int = PRIORITY_AUTO_KEEP


def sample_keep(trace_id: str, rate: float) -> bool:
    """The head-sampling decision: a pure function of ``(trace_id, rate)``.

    ``keep == (low64(trace_id) * 1111111111111111111 mod 2**64) < rate * 2**64``.
    Every service in a trace computes the same answer without talking to the
    others, and a downstream service inherits the decision anyway. ``rate * 2**64``
    is exact in a double (a power-of-two scaling), so Python ``int``, Node
    ``BigInt`` and Go ``uint64`` agree bit for bit (twin: ``wire.SampleKeep``).
    """
    if not rate > 0:  # also NaN
        return False
    if rate >= 1:
        return True
    if len(trace_id) != 32 or not _HEX.fullmatch(trace_id):
        return False
    low = int(trace_id[16:], 16)
    return (low * SAMPLING_MULTIPLIER) & _MASK64 < int(rate * (1 << 64))


def _is_id_segment(segment: str) -> bool:
    if not segment:
        return False
    return bool(
        _DIGITS.fullmatch(segment)
        or _UUID.fullmatch(segment)
        or _HEX12.fullmatch(segment)
        or (_NANO.fullmatch(segment) and _HAS_DIGIT.search(segment))
    )


def normalize_path(path: str) -> str:
    """Turn a URL path into a low-cardinality route resource (twin: ``wire.NormalizePath``).

    A segment that is all digits, a UUID, hex of 12 or more characters, or
    nanoid-like (16 or more of ``[A-Za-z0-9_-]`` containing a digit) becomes
    ``:id``; the query string and fragment are dropped; at most 8 segments are
    kept. It is the fallback for a request no route matched: the route pattern,
    when the framework knows one, is always better than a guess.
    """
    for i, ch in enumerate(path):
        if ch in "?#":
            path = path[:i]
            break
    if path in ("", "/"):
        return "/"
    segments = path.removeprefix("/").split("/")[:MAX_PATH_SEGMENTS]
    return "/" + "/".join(":id" if _is_id_segment(s) else s for s in segments)


def _text(value: Any) -> str | None:
    if isinstance(value, bytes | bytearray):
        try:
            return bytes(value).decode("ascii")
        except UnicodeDecodeError:
            return None
    if isinstance(value, bool):
        return None
    if isinstance(value, str | int):
        return str(value)
    return None


def _valid_id(value: str, length: int) -> bool:
    return len(value) == length and _HEX.fullmatch(value) is not None and value.strip("0") != ""


def parse_propagation(trace_id: Any, parent_id: Any, priority: Any = None) -> Context | None:
    """Read the three propagation values; ``None`` for anything malformed.

    Twin: ``wire.ParsePropagation``. Garbage never yields a context and never
    raises: the receiver starts a fresh trace instead of continuing a corrupt
    one. Uppercase hex is accepted and lowered; a missing priority means "keep"
    (1); a priority outside -1..2 is malformed. Values may be ``str`` or
    ``bytes`` (ASGI headers are bytes).
    """
    tid, pid = _text(trace_id), _text(parent_id)
    if tid is None or pid is None:
        return None
    tid, pid = tid.strip().lower(), pid.strip().lower()
    if not _valid_id(tid, 32) or not _valid_id(pid, 16):
        return None
    prio = PRIORITY_AUTO_KEEP
    if priority is not None:
        raw = _text(priority)
        if raw is None:
            return None
        raw = raw.strip()
        if raw:
            if raw not in {"-1", "0", "1", "2"}:
                return None
            prio = int(raw)
    return Context(tid, pid, prio)


def carrier_get(carrier: Mapping[Any, Any], key: str) -> Any:
    """Case-insensitive lookup in a header-like mapping (``str`` or ``bytes`` keys)."""
    if key in carrier:
        return carrier[key]
    for k, v in carrier.items():
        text = _text(k)
        if text is not None and text.lower() == key:
            return v
    return None
