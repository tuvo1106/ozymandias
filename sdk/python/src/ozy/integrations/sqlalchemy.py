"""SQLAlchemy: one ``postgres.query`` span per statement, from the engine's own events.

Mental model
------------
SQLAlchemy already announces every statement: ``before_cursor_execute`` fires just
before the DBAPI cursor runs it, ``after_cursor_execute`` just after, and
``handle_error`` when it raised. Listening there instead of wrapping a driver means the
integration works for every dialect and for async engines (an ``AsyncEngine`` runs its
statements on ``engine.sync_engine``), and a statement the ORM generates and one
you wrote by hand look the same.

The span is ``<dialect>.query`` (``postgres.query`` for PostgreSQL, ``sqlite.query``, ...),
type ``db``; the resource is the **statement text**, cut to 2000 characters.

What is never recorded
----------------------
**Parameters.** ``before_cursor_execute`` receives them and this module does not even
read them: a bound value is user data (emails, tokens), and the statement text with
``%s``/``?`` placeholders is what identifies the query anyway. The same care goes for
errors: a driver's message can quote the offending value (PostgreSQL's ``DETAIL: Key
(email)=(a@b.c) already exists``), so only the exception's *first line* is kept, and no
stack. The one thing it cannot protect is a statement an application built by pasting
values into the SQL string itself; that is in the text it was handed.

Only inside a trace
-------------------
A query with no active span (a migration, a startup probe, a background thread) is not
traced. A root ``postgres.query`` span would count as a service *entry* span in the
agent's request statistics, which is wrong.

Rejected alternative: patching ``Engine.__init__`` so every engine is instrumented
individually; listening on the ``Engine`` class gets the same effect with nothing to
keep in sync, and ``instrument(engine)`` covers the one-engine case.
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from typing import Any

from .. import tracer as _tracer
from . import register_integration
from ._patching import active_span

_log = logging.getLogger("ozy")

MAX_STATEMENT_CHARS = 2000
_SPAN_KEY = "_ozy_span"

_DIALECT_NAMES = {"postgresql": "postgres"}
_LISTENERS = ("before_cursor_execute", "after_cursor_execute", "handle_error")


def _target(engine: Any) -> Any:
    """The sync engine behind an ``AsyncEngine``, or the engine itself."""
    return getattr(engine, "sync_engine", engine)


def _before(
    conn: Any, cursor: Any, statement: str, parameters: Any, context: Any, executemany: bool
) -> None:
    try:
        if context is None or active_span() is None:
            return
        dialect = getattr(getattr(conn, "dialect", None), "name", "sql")
        span = _tracer.start_span(
            f"{_DIALECT_NAMES.get(dialect, dialect)}.query",
            resource=str(statement)[:MAX_STATEMENT_CHARS],
            type="db",
            tags={"db.system": dialect, "span.kind": "client"},
            activate=False,
        )
        database = getattr(getattr(conn, "engine", None), "url", None)
        database = getattr(database, "database", None)
        if database:
            span.set_tag("db.name", database)
        if executemany:
            span.set_tag("db.executemany", True)
        setattr(context, _SPAN_KEY, span)
    except Exception:
        _log.debug("ozy: starting a db span failed", exc_info=True)


def _after(
    conn: Any, cursor: Any, statement: str, parameters: Any, context: Any, executemany: bool
) -> None:
    try:
        span = getattr(context, _SPAN_KEY, None)
        if span is None:
            return
        setattr(context, _SPAN_KEY, None)
        rowcount = getattr(cursor, "rowcount", -1)
        if isinstance(rowcount, int) and rowcount >= 0:  # -1 means "the driver does not know"
            span.set_metric("db.rowcount", rowcount)
        span.finish()
    except Exception:
        _log.debug("ozy: finishing a db span failed", exc_info=True)


def _on_error(exception_context: Any) -> None:
    try:
        context = getattr(exception_context, "execution_context", None)
        span = getattr(context, _SPAN_KEY, None)
        if span is None:
            return
        setattr(context, _SPAN_KEY, None)
        original = getattr(exception_context, "original_exception", None)
        span.error = 1
        if original is not None:
            cls = type(original)
            span.set_tag("error.type", f"{cls.__module__}.{cls.__qualname__}")
            # First line only: see the module docstring on what a driver message can quote.
            first = str(original).splitlines()[0] if str(original) else ""
            span.set_tag("error.message", first)
        span.finish()
    except Exception:
        _log.debug("ozy: finishing a failed db span failed", exc_info=True)


class SqlAlchemyIntegration:
    """Trace every SQLAlchemy engine (``patch()``) or one engine (``instrument(engine)``)."""

    name = "sqlalchemy"

    def __init__(self) -> None:
        """Create an unpatched integration."""
        self._patched = False
        self._engines: list[Any] = []

    def is_available(self) -> bool:
        """SQLAlchemy 1.4+ (the ``handle_error`` event) is importable."""
        try:
            from sqlalchemy import event  # noqa: F401
            from sqlalchemy.engine import Engine  # noqa: F401
        except ImportError:
            return False
        return True

    def _listen(self, target: Any) -> None:
        from sqlalchemy import event

        event.listen(target, "before_cursor_execute", _before)
        event.listen(target, "after_cursor_execute", _after)
        event.listen(target, "handle_error", _on_error)

    def _remove(self, target: Any) -> None:
        from sqlalchemy import event

        fns: tuple[Callable[..., Any], ...] = (_before, _after, _on_error)
        for name, fn in zip(_LISTENERS, fns, strict=True):
            if event.contains(target, name, fn):
                event.remove(target, name, fn)

    def patch(self) -> None:
        """Listen on the ``Engine`` class: every engine, existing and future."""
        if self._patched or not self.is_available():
            return
        from sqlalchemy.engine import Engine

        self._listen(Engine)
        self._patched = True

    def unpatch(self) -> None:
        """Remove the class-level listeners and those of every ``instrument``-ed engine."""
        if self._patched:
            from sqlalchemy.engine import Engine

            self._remove(Engine)
            self._patched = False
        for engine in self._engines:
            self._remove(engine)
        self._engines.clear()

    def instrument(self, engine: Any) -> None:
        """Trace just this engine (an ``Engine`` or ``AsyncEngine``)."""
        target = _target(engine)
        if any(e is target for e in self._engines) or not self.is_available():
            return
        self._listen(target)
        self._engines.append(target)


INTEGRATION = SqlAlchemyIntegration()
register_integration(INTEGRATION)
instrument = INTEGRATION.instrument
