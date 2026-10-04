"""A ``logging`` formatter that writes one JSON object per line.

Mental model
------------
The agent can parse plain log lines with patterns, but a pattern is a guess
about text. A formatter that writes JSON removes the guess: the agent reads
``level``, ``timestamp``, ``message`` and ``trace_id`` as fields, every extra
you pass becomes a queryable attribute, and a traceback is one event, not
fifty lines to be re-joined by a heuristic.

Usage::

    import logging
    from ozy.integrations.logging import JSONFormatter

    handler = logging.StreamHandler()
    handler.setFormatter(JSONFormatter())
    logging.getLogger().addHandler(handler)

    logging.getLogger("shop").info("order placed", extra={"order_id": 42, "total": 19.5})

emits (one line)::

    {"timestamp":"2026-10-04T12:00:00.123Z","level":"info","message":"order placed",
     "logger":"shop","order_id":42,"total":19.5}

Two rules make it safe to leave on in production:

* **It never raises.** A formatter that throws takes the log call with it, and
  logging is the last place an app can afford an exception. A record it cannot
  serialize becomes a line that says so; the fields it can serialize are kept.
* **It never writes two lines.** JSON escapes every newline, so a message with
  embedded newlines and a full traceback are still one line, and a line-based
  reader (the agent's tailer, ``docker logs``, ``grep``) sees one event.

Fields
------
``timestamp`` (UTC, ISO 8601, milliseconds), ``level`` (lowercase), ``message``,
``logger`` and, when there is one, ``exception`` (the formatted traceback),
``error.kind`` (the exception class), ``trace_id`` and ``span_id`` (from the
record, set by the tracer or by ``extra=``), and ``service``/``env``/``version``
when given or present in ``OZY_SERVICE``/``OZY_ENV``/``OZY_VERSION``.
Everything else in ``extra=`` is added as a field of the same name, so it is
queryable as ``@name:value``. An extra that collides with one of the fields
above is kept under ``<name>_`` rather than replacing it: the fields above are
what the pipeline relies on.
"""

from __future__ import annotations

import datetime
import json
import logging
import os
from collections.abc import Mapping
from typing import Any

__all__ = ["JSONFormatter"]

# Attributes every LogRecord has. Anything else on a record came from ``extra=``
# (or a filter), and is a field of the log.
_STANDARD = frozenset(logging.LogRecord("", 0, "", 0, "", (), None).__dict__.keys()) | {
    "message",
    "asctime",
    "taskName",
}

_RESERVED = frozenset(
    {
        "timestamp",
        "level",
        "message",
        "logger",
        "exception",
        "error.kind",
        "trace_id",
        "span_id",
        "service",
        "env",
        "version",
    }
)


class JSONFormatter(logging.Formatter):
    """Format a :class:`logging.LogRecord` as one line of JSON.

    Args:
        service, env, version: Added to every line when given. Default: the
            ``OZY_SERVICE``, ``OZY_ENV`` and ``OZY_VERSION`` environment
            variables, if set, read once at construction.
        static: Extra fixed fields added to every line (for instance
            ``{"component": "worker"}``).
    """

    def __init__(
        self,
        *,
        service: str | None = None,
        env: str | None = None,
        version: str | None = None,
        static: Mapping[str, Any] | None = None,
    ) -> None:
        """Build a formatter; see the class docstring for the arguments."""
        super().__init__()
        self._static: dict[str, Any] = {}
        for key, given in (("service", service), ("env", env), ("version", version)):
            value = given if given is not None else os.environ.get(f"OZY_{key.upper()}")
            if value:
                self._static[key] = value
        if static:
            self._static.update(static)

    def format(self, record: logging.LogRecord) -> str:
        """Return the record as one line of JSON; never raises."""
        try:
            return self._encode(self._fields(record))
        except Exception as exc:
            return self._fallback(record, exc)

    # -- internals ---------------------------------------------------------------

    def _fields(self, record: logging.LogRecord) -> dict[str, Any]:
        out: dict[str, Any] = {
            "timestamp": _iso(record.created),
            "level": record.levelname.lower(),
            "message": record.getMessage(),
            "logger": record.name,
        }
        for key, value in self._static.items():
            out.setdefault(key, value)
        if record.exc_info and record.exc_info[0] is not None:
            out["exception"] = self.formatException(record.exc_info)
            out["error.kind"] = record.exc_info[0].__name__
        elif record.exc_text:
            out["exception"] = record.exc_text
        if record.stack_info:
            out["stack"] = self.formatStack(record.stack_info)
        for key, value in record.__dict__.items():
            if key in _STANDARD:
                continue
            if key in ("trace_id", "span_id"):
                if value:
                    out[key] = str(value)
                continue
            out[key + "_" if key in _RESERVED or key in out else key] = value
        return out

    @staticmethod
    def _encode(fields: dict[str, Any]) -> str:
        try:
            return json.dumps(fields, ensure_ascii=False, default=_default, separators=(",", ":"))
        except (TypeError, ValueError):
            # An unpaired surrogate cannot be written as UTF-8; escaping it can.
            return json.dumps(fields, ensure_ascii=True, default=_default, separators=(",", ":"))

    @staticmethod
    def _fallback(record: logging.LogRecord, exc: Exception) -> str:
        try:
            safe: dict[str, Any] = {
                "timestamp": _iso(record.created),
                "level": str(record.levelname).lower(),
                "message": _safe_message(record),
                "logger": str(record.name),
                "format_error": f"{type(exc).__name__}: {exc}",
            }
            return json.dumps(safe, ensure_ascii=True, default=str, separators=(",", ":"))
        except Exception:
            return '{"level":"error","message":"ozy: a log record could not be formatted"}'


def _safe_message(record: logging.LogRecord) -> str:
    try:
        return record.getMessage()
    except Exception:
        return str(getattr(record, "msg", ""))


def _iso(created: float) -> str:
    dt = datetime.datetime.fromtimestamp(created, tz=datetime.UTC)
    return dt.strftime("%Y-%m-%dT%H:%M:%S.") + f"{dt.microsecond // 1000:03d}Z"


def _default(value: Any) -> Any:
    """What json cannot encode: a repr is better than losing the line."""
    if isinstance(value, (set, frozenset)):
        return sorted(value, key=str)
    if isinstance(value, (bytes, bytearray)):
        return bytes(value).decode("utf-8", "replace")
    if isinstance(value, BaseException):
        return f"{type(value).__name__}: {value}"
    if isinstance(value, (datetime.datetime, datetime.date)):
        return value.isoformat()
    return repr(value)
