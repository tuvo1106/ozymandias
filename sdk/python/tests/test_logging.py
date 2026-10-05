"""JSONFormatter: one line, never raises, fields as documented."""

from __future__ import annotations

import io
import json
import logging

import pytest

from ozy.integrations.logging import JSONFormatter


def render(
    message: str = "hello",
    *args: object,
    level: int = logging.INFO,
    formatter: JSONFormatter | None = None,
    **extra: object,
) -> str:
    stream = io.StringIO()
    handler = logging.StreamHandler(stream)
    handler.setFormatter(formatter or JSONFormatter())
    logger = logging.getLogger(f"test.{id(stream)}")
    logger.handlers = [handler]
    logger.propagate = False
    logger.setLevel(logging.DEBUG)
    logger.log(level, message, *args, extra=extra or None)
    return stream.getvalue()


def one(line: str) -> dict[str, object]:
    assert line.endswith("\n")
    assert line.count("\n") == 1, f"not exactly one line: {line!r}"
    parsed: dict[str, object] = json.loads(line)
    return parsed


def test_standard_fields() -> None:
    out = one(render("order placed", level=logging.WARNING))
    assert out["message"] == "order placed"
    assert out["level"] == "warning"
    assert str(out["logger"]).startswith("test.")
    ts = str(out["timestamp"])
    assert len(ts) == 24
    assert ts.endswith("Z")
    assert ts[10] == "T"
    assert ts[19] == "."


def test_message_arguments_are_formatted() -> None:
    assert one(render("%s paid %d", "ann", 3))["message"] == "ann paid 3"


def test_extras_become_fields_and_keep_their_json_types() -> None:
    out = one(
        render("x", order_id=42, total=19.5, paid=True, tags=["a", "b"], meta={"k": 1}, none=None)
    )
    assert out["order_id"] == 42
    assert out["total"] == 19.5
    assert out["paid"] is True
    assert out["tags"] == ["a", "b"]
    assert out["meta"] == {"k": 1}
    assert out["none"] is None


def test_an_extra_cannot_overwrite_a_field_the_pipeline_relies_on() -> None:
    out = one(
        render("real message", level_="x", status="warn", trace_id="a" * 32, span_id="b" * 16)
    )
    # `level` and `message` are not valid `extra` keys for logging itself; the
    # reserved ones that are allowed are kept apart rather than replacing ours.
    assert out["message"] == "real message"
    assert out["trace_id"] == "a" * 32
    assert out["span_id"] == "b" * 16
    assert out["status"] == "warn"  # not reserved: the agent moves a numeric one to status_code
    renamed = one(render("m", version="9"))
    assert renamed["version_"] == "9"


def test_trace_ids_are_included_only_when_set() -> None:
    assert "trace_id" not in one(render("x"))
    assert "trace_id" not in one(render("x", trace_id=""))
    assert one(render("x", trace_id=123))["trace_id"] == "123"


def test_exceptions_are_one_event_not_many_lines() -> None:
    stream = io.StringIO()
    handler = logging.StreamHandler(stream)
    handler.setFormatter(JSONFormatter())
    logger = logging.getLogger("test.exc")
    logger.handlers, logger.propagate = [handler], False
    try:
        raise ValueError("no good\nreally")
    except ValueError:
        logger.exception("failed")
    out = one(stream.getvalue())
    assert out["error.kind"] == "ValueError"
    assert "Traceback (most recent call last)" in str(out["exception"])
    assert "ValueError: no good" in str(out["exception"])
    assert out["message"] == "failed"


def test_newlines_in_a_message_stay_on_one_line() -> None:
    assert one(render("a\nb\r\nc"))["message"] == "a\nb\r\nc"


def test_service_env_version_from_arguments_or_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    assert "service" not in one(render("x"))
    monkeypatch.setenv("OZY_SERVICE", "web-api")
    monkeypatch.setenv("OZY_ENV", "dev")
    out = one(render("x", formatter=JSONFormatter()))
    assert out["service"] == "web-api"
    assert out["env"] == "dev"
    out = one(
        render(
            "x", formatter=JSONFormatter(service="worker", version="1.2", static={"component": "q"})
        )
    )
    assert out["service"] == "worker"
    assert out["version"] == "1.2"
    assert out["component"] == "q"
    assert out["env"] == "dev"


def test_values_json_cannot_encode_do_not_lose_the_line() -> None:
    class Odd:
        def __repr__(self) -> str:
            return "<odd>"

    out = one(
        render("x", thing=Odd(), tags={"b", "a"}, raw=b"by\xffte", err=KeyError("k"), cycle=object)
    )
    assert out["thing"] == "<odd>"
    assert out["tags"] == ["a", "b"]
    assert str(out["raw"]).startswith("by")
    assert "KeyError" in str(out["err"])


def test_unicode_is_kept_and_a_lone_surrogate_does_not_raise() -> None:
    assert one(render("café ☕ 日本"))["message"] == "café ☕ 日本"
    line = render("bad \ud800 surrogate")
    assert json.loads(line)["message"] == "bad \ud800 surrogate"


def test_a_broken_format_string_does_not_raise() -> None:
    record = logging.LogRecord("n", logging.INFO, __file__, 1, "%d items", ("not a number",), None)
    out = one(JSONFormatter().format(record) + "\n")
    assert out["message"] == "%d items"
    assert "TypeError" in str(out["format_error"])


def test_a_failing_str_in_an_extra_does_not_raise() -> None:
    class Bad:
        def __repr__(self) -> str:
            raise RuntimeError("boom")

    out = one(JSONFormatter().format(_record(bad=Bad())) + "\n")
    assert out["message"] == "x"
    assert "format_error" in out


def _record(**extra: object) -> logging.LogRecord:
    r = logging.LogRecord("n", logging.INFO, __file__, 1, "x", (), None)
    r.__dict__.update(extra)
    return r


def test_stack_info_and_exc_text() -> None:
    r = _record()
    r.stack_info = "Stack (most recent call last):\n  File x"
    r.exc_text = "cached traceback text"
    out = one(JSONFormatter().format(r) + "\n")
    assert out["exception"] == "cached traceback text"
    assert "Stack" in str(out["stack"])


def test_exc_info_with_no_exception_is_ignored() -> None:
    r = _record()
    r.exc_info = (None, None, None)
    assert "exception" not in one(JSONFormatter().format(r) + "\n")


def test_even_the_fallback_cannot_raise() -> None:
    record = _record()
    record.created = "not a time"  # type: ignore[assignment]
    line = JSONFormatter().format(record)
    assert json.loads(line)["level"] == "error"
