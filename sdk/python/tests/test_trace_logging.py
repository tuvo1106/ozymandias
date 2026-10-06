"""Log <-> trace correlation: the filter / record factory stamps ids, JSONFormatter emits them."""

from __future__ import annotations

import io
import json
import logging
from collections.abc import Iterator
from typing import Any

import pytest

import ozy
from ozy.integrations.logging import INTEGRATION, JSONFormatter, TraceLogFilter

from .conftest import FakeTraceAgent


@pytest.fixture
def stream() -> Iterator[io.StringIO]:
    out = io.StringIO()
    handler = logging.StreamHandler(out)
    handler.setFormatter(JSONFormatter())
    logger = logging.getLogger("ozy.test.trace")
    logger.handlers = [handler]
    logger.propagate = False
    logger.setLevel(logging.INFO)
    yield out
    logger.handlers = []


def lines(stream: io.StringIO) -> list[dict[str, Any]]:
    return [json.loads(line) for line in stream.getvalue().splitlines()]


def test_a_handler_filter_stamps_the_active_span_ids(
    traced: FakeTraceAgent, stream: io.StringIO
) -> None:
    logger = logging.getLogger("ozy.test.trace")
    logger.handlers[0].addFilter(TraceLogFilter())
    with ozy.tracer.trace("request") as span:
        logger.info("inside")
    logger.info("outside")
    inside, outside = lines(stream)
    assert inside["trace_id"] == span.trace_id
    assert inside["span_id"] == span.span_id
    assert "trace_id" not in outside  # no span, no fields: nothing to filter on in the UI
    assert "span_id" not in outside


def test_the_filter_never_rejects_a_record(traced: FakeTraceAgent) -> None:
    record = logging.LogRecord("x", logging.INFO, __file__, 1, "m", (), None)
    assert TraceLogFilter().filter(record) is True
    assert not hasattr(record, "trace_id")


def test_the_filter_keeps_an_explicit_trace_id(traced: FakeTraceAgent) -> None:
    record = logging.LogRecord("x", logging.INFO, __file__, 1, "m", (), None)
    record.trace_id = "explicit"
    with ozy.tracer.trace("request"):
        TraceLogFilter().filter(record)
    assert getattr(record, "trace_id", None) == "explicit"


def test_the_record_factory_covers_every_logger_and_handler(
    traced: FakeTraceAgent, stream: io.StringIO
) -> None:
    INTEGRATION.patch()
    INTEGRATION.patch()  # idempotent: a second patch must not wrap the wrapper
    child = logging.getLogger("ozy.test.trace.child.deeper")  # no handler of its own
    with ozy.tracer.trace("request") as span:
        child.info("from a child logger")
    (line,) = lines(stream)
    assert line["trace_id"] == span.trace_id
    assert line["span_id"] == span.span_id


def test_unpatch_restores_the_previous_factory(traced: FakeTraceAgent) -> None:
    before = logging.getLogRecordFactory()
    INTEGRATION.patch()
    assert logging.getLogRecordFactory() is not before
    INTEGRATION.unpatch()
    INTEGRATION.unpatch()
    assert logging.getLogRecordFactory() is before


def test_the_factory_never_breaks_a_log_call(
    traced: FakeTraceAgent, monkeypatch: pytest.MonkeyPatch
) -> None:
    INTEGRATION.patch()

    def boom() -> Any:
        raise RuntimeError("tracer is broken")

    monkeypatch.setattr(ozy.tracer, "current_span", boom)
    record = logging.getLogRecordFactory()("x", logging.INFO, __file__, 1, "m", (), None)
    assert record.getMessage() == "m"


def test_logging_integration_metadata() -> None:
    assert INTEGRATION.name == "logging"
    assert INTEGRATION.is_available()
