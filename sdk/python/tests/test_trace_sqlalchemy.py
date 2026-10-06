"""SQLAlchemy integration against a real engine (SQLite): statement text yes, parameters never."""

from __future__ import annotations

from types import SimpleNamespace
from typing import Any

import pytest
from sqlalchemy import create_engine, event, text
from sqlalchemy.engine import Engine
from sqlalchemy.exc import IntegrityError

import ozy
from ozy.integrations import sqlalchemy as ozy_sa
from ozy.integrations.sqlalchemy import INTEGRATION, MAX_STATEMENT_CHARS

from .conftest import FakeTraceAgent

SENTINEL = "param-sentinel-7f3a91c2"


@pytest.fixture
def engine() -> Engine:
    eng = create_engine("sqlite://")
    with eng.begin() as conn:
        conn.execute(text("CREATE TABLE users (id INTEGER PRIMARY KEY, email TEXT UNIQUE)"))
    return eng


def db_spans(agent: FakeTraceAgent) -> list[dict[str, Any]]:
    ozy.tracer.flush()
    return [s for s in agent.spans() if s["type"] == "db"]


def test_a_query_inside_a_trace_becomes_a_db_span(traced: FakeTraceAgent, engine: Engine) -> None:
    INTEGRATION.patch()
    with ozy.tracer.trace("request") as root, engine.begin() as conn:
        conn.execute(text("INSERT INTO users (email) VALUES (:e)"), {"e": SENTINEL})
        rows = conn.execute(text("SELECT id, email FROM users WHERE email = :e"), {"e": SENTINEL})
        assert rows.fetchall()
    spans = db_spans(traced)
    insert = next(s for s in spans if s["resource"].startswith("INSERT"))
    select = next(s for s in spans if s["resource"].startswith("SELECT"))
    assert insert["name"] == select["name"] == "sqlite.query"  # named for the dialect
    assert insert["resource"] == "INSERT INTO users (email) VALUES (?)"  # placeholders, not values
    assert select["parent_id"] == root.span_id
    assert select["trace_id"] == root.trace_id
    assert insert["meta"]["db.system"] == "sqlite"
    assert insert["meta"]["span.kind"] == "client"
    assert insert["metrics"]["db.rowcount"] == 1
    assert insert["error"] == 0
    assert "_top_level" not in insert["metrics"]  # a db span is never a service entry


def test_no_parameter_value_appears_anywhere_in_the_payload(
    traced: FakeTraceAgent, engine: Engine
) -> None:
    INTEGRATION.patch()
    with ozy.tracer.trace("request"), engine.begin() as conn:
        conn.execute(text("INSERT INTO users (email) VALUES (:e)"), {"e": SENTINEL})
        conn.execute(text("SELECT * FROM users WHERE email = :e"), {"e": SENTINEL})
        conn.execute(
            text("INSERT INTO users (email) VALUES (:e)"),
            [{"e": SENTINEL + "1"}, {"e": SENTINEL + "2"}],
        )
    ozy.tracer.flush()
    assert traced.raw
    assert all(SENTINEL.encode() not in body for body in traced.raw)
    assert len(db_spans(traced)) >= 3


def test_a_failing_statement_marks_the_span_and_still_leaks_no_parameter(
    traced: FakeTraceAgent, engine: Engine
) -> None:
    INTEGRATION.patch()
    with engine.begin() as conn:
        conn.execute(text("INSERT INTO users (email) VALUES (:e)"), {"e": SENTINEL})
    with ozy.tracer.trace("request"), pytest.raises(IntegrityError) as raised:
        with engine.begin() as conn:
            conn.execute(text("INSERT INTO users (email) VALUES (:e)"), {"e": SENTINEL})
    # SQLAlchemy's own message quotes the parameters; that is exactly why the span does not use it.
    assert SENTINEL in str(raised.value)
    failed = [s for s in db_spans(traced) if s["error"] == 1]
    assert len(failed) == 1
    assert failed[0]["meta"]["error.type"] == "sqlite3.IntegrityError"
    assert "UNIQUE constraint failed" in failed[0]["meta"]["error.message"]
    assert "error.stack" not in failed[0]["meta"]
    assert all(SENTINEL.encode() not in body for body in traced.raw)


def test_only_the_first_line_of_a_driver_error_is_kept() -> None:
    # PostgreSQL appends "DETAIL: Key (email)=(<value>) already exists": the value is user data.
    ctx = SimpleNamespace()
    tags: dict[str, Any] = {}
    span: Any = SimpleNamespace(tags=tags, error=0, finished=False, set_tag=tags.__setitem__)
    span.finish = lambda: setattr(span, "finished", True)
    ctx._ozy_span = span
    exc = Exception(f'duplicate key value violates unique constraint "x"\nDETAIL: Key ({SENTINEL})')
    ozy_sa._on_error(SimpleNamespace(execution_context=ctx, original_exception=exc))
    assert span.tags["error.message"] == 'duplicate key value violates unique constraint "x"'
    assert SENTINEL not in str(span.tags)
    assert span.error == 1
    assert span.finished


def test_a_long_statement_is_truncated_to_2000_characters(
    traced: FakeTraceAgent, engine: Engine
) -> None:
    INTEGRATION.patch()
    sql = "SELECT 1 /* " + "x" * 5000 + " */"
    with ozy.tracer.trace("request"), engine.connect() as conn:
        conn.execute(text(sql))
    (span,) = db_spans(traced)
    assert len(span["resource"]) == MAX_STATEMENT_CHARS
    assert span["resource"].startswith("SELECT 1 /* xxx")


def test_executemany_is_flagged(traced: FakeTraceAgent, engine: Engine) -> None:
    INTEGRATION.patch()
    with ozy.tracer.trace("request"), engine.begin() as conn:
        conn.execute(text("INSERT INTO users (email) VALUES (:e)"), [{"e": "a"}, {"e": "b"}])
    (span,) = [s for s in db_spans(traced) if s["resource"].startswith("INSERT")]
    assert span["meta"]["db.executemany"] == "true"


def test_a_query_outside_any_trace_is_not_traced(traced: FakeTraceAgent, engine: Engine) -> None:
    INTEGRATION.patch()
    with engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert db_spans(traced) == []  # a root db span would count as a service entry


def test_patch_is_idempotent_and_unpatch_removes_the_listeners(
    traced: FakeTraceAgent, engine: Engine
) -> None:
    INTEGRATION.patch()
    INTEGRATION.patch()
    with ozy.tracer.trace("request"), engine.connect() as conn:
        conn.execute(text("SELECT 1"))
    assert len(db_spans(traced)) == 1  # not one per patch() call
    INTEGRATION.unpatch()
    INTEGRATION.unpatch()
    assert not event.contains(Engine, "before_cursor_execute", ozy_sa._before)
    with ozy.tracer.trace("request"), engine.connect() as conn:
        conn.execute(text("SELECT 2"))
    assert len(db_spans(traced)) == 1


def test_instrument_traces_only_that_engine(traced: FakeTraceAgent, engine: Engine) -> None:
    other = create_engine("sqlite://")
    INTEGRATION.instrument(engine)
    INTEGRATION.instrument(engine)  # idempotent
    with ozy.tracer.trace("request"):
        with other.connect() as conn:
            conn.execute(text("SELECT 'other'"))
        with engine.connect() as conn:
            conn.execute(text("SELECT 'mine'"))
    spans = db_spans(traced)
    assert [s["resource"] for s in spans] == ["SELECT 'mine'"]
    INTEGRATION.unpatch()  # removes per-engine listeners too
    with ozy.tracer.trace("again"), engine.connect() as conn:
        conn.execute(text("SELECT 'mine again'"))
    assert len(db_spans(traced)) == 1


def test_an_async_engine_is_instrumented_through_its_sync_engine(engine: Engine) -> None:
    assert ozy_sa._target(SimpleNamespace(sync_engine=engine)) is engine
    assert ozy_sa._target(engine) is engine


def test_listeners_swallow_their_own_failures(traced: FakeTraceAgent) -> None:
    # Hostile arguments: the host's query must run whatever our listeners make of them.
    ozy_sa._before(object(), object(), "SELECT 1", (), None, False)
    ozy_sa._before(object(), object(), "SELECT 1", (), SimpleNamespace(), False)
    ozy_sa._after(object(), object(), "SELECT 1", (), object(), False)
    ozy_sa._on_error(object())
    with ozy.tracer.trace("request"):
        ozy_sa._before(None, None, "SELECT 1", (), SimpleNamespace(), False)
        ozy_sa._after(None, SimpleNamespace(rowcount="many"), "x", (), SimpleNamespace(), False)


@pytest.mark.parametrize(
    ("dialect", "name"),
    [("postgresql", "postgres.query"), ("mysql", "mysql.query"), ("sqlite", "sqlite.query")],
)
def test_the_span_is_named_for_the_dialect(traced: FakeTraceAgent, dialect: str, name: str) -> None:
    conn = SimpleNamespace(dialect=SimpleNamespace(name=dialect), engine=None)
    with ozy.tracer.trace("request"):
        ctx = SimpleNamespace()
        ozy_sa._before(conn, None, "SELECT 1", (), ctx, False)
        ozy_sa._after(conn, SimpleNamespace(rowcount=-1), "SELECT 1", (), ctx, False)
    (span,) = db_spans(traced)
    assert span["name"] == name
    assert span["meta"]["db.system"] == dialect
    assert "db.rowcount" not in span["metrics"]  # -1 means the driver does not know


def test_the_database_name_is_recorded_without_credentials(traced: FakeTraceAgent) -> None:
    url = SimpleNamespace(database="shop", password="hunter2")
    conn = SimpleNamespace(
        dialect=SimpleNamespace(name="postgresql"), engine=SimpleNamespace(url=url)
    )
    with ozy.tracer.trace("request"):
        ctx = SimpleNamespace()
        ozy_sa._before(conn, None, "SELECT 1", (), ctx, False)
        ozy_sa._after(conn, SimpleNamespace(rowcount=1), "SELECT 1", (), ctx, False)
    (span,) = db_spans(traced)
    assert span["meta"]["db.name"] == "shop"
    assert all(b"hunter2" not in body for body in traced.raw)


def test_unavailable_library_is_a_noop(monkeypatch: pytest.MonkeyPatch) -> None:
    import builtins

    real = builtins.__import__

    def fake(name: str, *a: Any, **k: Any) -> Any:
        if name.startswith("sqlalchemy"):
            raise ImportError(name)
        return real(name, *a, **k)

    monkeypatch.setattr(builtins, "__import__", fake)
    assert INTEGRATION.is_available() is False
    INTEGRATION.patch()  # no raise, nothing patched
    INTEGRATION.instrument(object())
    assert INTEGRATION._patched is False
