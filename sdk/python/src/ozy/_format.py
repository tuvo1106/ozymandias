"""extended StatsD line formatting: the one place that decides which bytes go on the wire.

The format is specified in ``docs/wire-protocol.md`` §A ("What a client
sends") and pinned byte-for-byte by ``pkg/wire/testdata/statsd/sdk-cases.json``,
which the Go parser and both SDK test suites load. Everything here is a pure
function so the contract tests can exercise it without sockets or threads.

Two rules shape this module:

* **The client sanitizes as little as possible.** Only the characters that
  would corrupt the *framing* of a datagram are replaced: ``|`` (section
  separator), ``,`` (tag separator) and newline (message separator), plus
  ``:`` in names because it ends the name. Lowercasing, length limits and
  character sets are the agent's job, so there is exactly one normalizer of
  record instead of one per SDK that could drift apart.
* **Output is deterministic.** Section order is fixed (rate before tags) and
  numbers print the same way in every SDK, so goldens can compare bytes.
"""

from __future__ import annotations

import math
from collections.abc import Iterable

# str.translate tables are the fastest way to do several single-character
# replacements in CPython: one C-level pass instead of one .replace() each.
_NAME_TABLE = str.maketrans({"|": "_", ",": "_", "\n": "_", ":": "_"})
_TAG_TABLE = str.maketrans({"|": "_", ",": "_", "\n": "_"})


def sanitize_name(name: str) -> str:
    """Make a metric name safe to frame: ``|``, ``,``, ``:`` and newline become ``_``."""
    return name.translate(_NAME_TABLE)


def sanitize_tag(tag: str) -> str:
    """Make a tag (or set member) safe to frame: ``|``, ``,`` and newline become ``_``.

    ``:`` is kept because it separates a tag's key from its value, and a set
    member comes after the name's ``:`` so it cannot end anything.
    """
    return tag.translate(_TAG_TABLE)


def format_number(value: object) -> str | None:
    """Render a number the way every ozymandias SDK does, or ``None`` if unsendable.

    Integral values print without a decimal point (``1``, not ``1.0``) and
    everything else as the shortest string that round-trips a float64 —
    which is exactly what ``repr(float)`` produces, so the rule reduces to
    "``repr``, minus a trailing ``.0``". NaN and ±Inf return ``None``: the
    agent would reject the line anyway, and dropping it here means one bad
    value never costs the other messages in its datagram.

    Args:
        value: Anything ``float()`` accepts (int, float, Decimal, Fraction…).
            ``bool`` is accepted as 0/1 because it is an ``int`` subclass.

    Returns:
        The decimal string, or ``None`` for non-finite values.

    Raises:
        TypeError: ``value`` is not a number (e.g. a string).
        ValueError: ``value`` cannot be converted to a float.
        OverflowError: ``value`` is an int too large for a float64.
    """
    if isinstance(value, str):
        # float("3") would succeed, silently accepting a type mistake that
        # every other SDK rejects. Be strict so behaviour matches.
        raise TypeError("metric value must be a number, not str")
    f = float(value)  # type: ignore[arg-type]  # guarded by callers' error handling
    if not math.isfinite(f):
        return None
    if f == 0:
        return "0"  # normalizes -0.0, which repr would print as "-0"
    text = repr(f)
    # repr prints integral floats below 1e16 as "3.0" and above as "1e+16";
    # only the first needs trimming to satisfy "no decimal point".
    return text[:-2] if text.endswith(".0") else text


def format_line(
    name: str,
    value: str,
    metric_type: str,
    sample_rate: float,
    tags: Iterable[str],
) -> str:
    """Assemble one extended StatsD message: ``name:value|type[|@rate][|#tags]``.

    ``name`` is sanitized here; ``value`` and ``tags`` must already be
    formatted and sanitized (the caller combines call tags with the
    precomputed global tags, which are sanitized once at ``init()``).
    ``|@rate`` is written only when ``sample_rate < 1`` because a rate of 1
    is the agent's default and the goldens expect it omitted.
    """
    line = f"{sanitize_name(name)}:{value}|{metric_type}"
    if sample_rate < 1:
        rate = format_number(sample_rate)
        line += f"|@{rate}"
    joined = ",".join(tags)
    if joined:
        line += f"|#{joined}"
    return line
