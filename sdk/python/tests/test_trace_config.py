"""Tracing configuration (``OZY_TRACE_*``) and its wiring through ``ozy.init``."""

from __future__ import annotations

import logging
from typing import Any

import pytest

import ozy
from ozy._config import resolve_config

from .conftest import FakeAgent, FakeTraceAgent


def test_trace_defaults() -> None:
    cfg = resolve_config(environ={})
    assert cfg.trace_enabled is True  # "on" still needs an agent host to mean anything
    assert cfg.trace_port == 8126
    assert cfg.trace_sample_rate == 1.0
    assert cfg.traces_enabled is False
    assert resolve_config(agent_host="h", environ={}).traces_enabled is True


def test_trace_env_vars() -> None:
    cfg = resolve_config(
        environ={
            "OZY_AGENT_HOST": "h",
            "OZY_TRACE_ENABLED": "false",
            "OZY_TRACE_PORT": "9126",
            "OZY_TRACE_SAMPLE_RATE": "0.25",
        }
    )
    assert (cfg.trace_enabled, cfg.trace_port, cfg.trace_sample_rate) == (False, 9126, 0.25)
    assert cfg.traces_enabled is False  # an agent is configured but tracing is switched off


@pytest.mark.parametrize("raw", ["0", "false", "FALSE", "no", "off", " off "])
def test_trace_enabled_falsy_spellings(raw: str) -> None:
    assert resolve_config(environ={"OZY_TRACE_ENABLED": raw}).trace_enabled is False


@pytest.mark.parametrize("raw", ["1", "true", "yes", "on", "", "garbage"])
def test_trace_enabled_everything_else_stays_on(raw: str) -> None:
    assert resolve_config(environ={"OZY_TRACE_ENABLED": raw}).trace_enabled is True


def test_trace_arguments_override_env() -> None:
    cfg = resolve_config(
        trace_enabled=True,
        trace_port=1234,
        trace_sample_rate=0.5,
        environ={"OZY_TRACE_ENABLED": "0", "OZY_TRACE_PORT": "9", "OZY_TRACE_SAMPLE_RATE": "0.9"},
    )
    assert (cfg.trace_enabled, cfg.trace_port, cfg.trace_sample_rate) == (True, 1234, 0.5)


@pytest.mark.parametrize(
    ("raw", "want"),
    [
        ("2", 1.0),
        ("-1", 0.0),
        ("0", 0.0),
        ("nan", 1.0),
        ("abc", 1.0),
        ("inf", 1.0),
        ("0.001", 0.001),
    ],
)
def test_trace_sample_rate_is_clamped_and_bad_values_fall_back(raw: str, want: float) -> None:
    assert resolve_config(environ={"OZY_TRACE_SAMPLE_RATE": raw}).trace_sample_rate == want


def test_trace_sample_rate_argument_is_clamped_too() -> None:
    assert resolve_config(trace_sample_rate=7, environ={}).trace_sample_rate == 1.0
    assert resolve_config(trace_sample_rate=float("nan"), environ={}).trace_sample_rate == 1.0
    assert resolve_config(trace_sample_rate=-3, environ={}).trace_sample_rate == 0.0


@pytest.mark.parametrize("raw", ["abc", "0", "70000", "-1"])
def test_bad_trace_port_falls_back_to_default(raw: str) -> None:
    assert resolve_config(environ={"OZY_TRACE_PORT": raw}).trace_port == 8126


@pytest.mark.parametrize("port", [0, 70000, -5])
def test_bad_trace_port_argument_falls_back_to_default(port: int) -> None:
    assert resolve_config(trace_port=port, environ={}).trace_port == 8126


# -- init --------------------------------------------------------------------------


def test_tracer_is_one_stable_object() -> None:
    from ozy import tracer as tracer_again

    assert ozy.tracer is tracer_again
    before = ozy.tracer
    ozy.init(agent_host="127.0.0.1")
    assert ozy.tracer is before
    assert ozy.tracer.enabled


def test_init_without_an_agent_host_leaves_tracing_inert() -> None:
    from ozy.integrations.logging import INTEGRATION

    ozy.init(service="x", integrations=["logging"])
    assert not ozy.tracer.enabled
    with ozy.tracer.trace("op") as span:
        assert span.span_id == "0" * 16  # the shared no-op span
    assert INTEGRATION._previous is None  # nothing was patched either


def test_init_from_env_enables_tracing_end_to_end(
    monkeypatch: pytest.MonkeyPatch, trace_agent: FakeTraceAgent
) -> None:
    monkeypatch.setenv("OZY_AGENT_HOST", "127.0.0.1")
    monkeypatch.setenv("OZY_TRACE_PORT", str(trace_agent.port))
    monkeypatch.setenv("OZY_SERVICE", "env-svc")
    monkeypatch.setenv("OZY_TRACE_SAMPLE_RATE", "0")
    ozy.init(env="dev", version="9")
    with ozy.tracer.trace("op"):
        pass
    ozy.tracer.flush()
    (span,) = trace_agent.spans()
    assert span["service"] == "env-svc"
    assert span["meta"] == {"env": "dev", "version": "9"}
    assert span["metrics"]["_sampling_priority"] == 0


def test_trace_enabled_false_keeps_metrics_on(
    monkeypatch: pytest.MonkeyPatch, agent: FakeAgent
) -> None:
    monkeypatch.setenv("OZY_TRACE_ENABLED", "0")
    ozy.init(agent_host="127.0.0.1", statsd_port=agent.port)
    assert ozy.statsd.enabled
    assert not ozy.tracer.enabled


def test_init_patches_the_named_integrations_when_tracing_is_on(
    trace_agent: FakeTraceAgent, caplog: pytest.LogCaptureFixture
) -> None:
    from ozy.integrations import get_integration
    from ozy.integrations.logging import INTEGRATION

    with caplog.at_level(logging.WARNING, logger="ozy"):
        ozy.init(
            agent_host="127.0.0.1",
            trace_port=trace_agent.port,
            integrations=["logging", "no-such-integration"],  # logged, never raised
        )
    assert INTEGRATION._previous is not None
    assert get_integration("no-such-integration") is None
    assert "unknown integration" in caplog.text
    ozy.init(agent_host="127.0.0.1", trace_port=trace_agent.port, integrations=["logging"])


def test_init_failure_never_raises_and_leaves_tracing_disabled(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    calls = {"n": 0}
    real = ozy.Tracer.configure

    def flaky(self: Any, config: Any) -> None:
        calls["n"] += 1
        if config.traces_enabled:
            raise RuntimeError("boom")
        real(self, config)

    monkeypatch.setattr(ozy.Tracer, "configure", flaky)
    with caplog.at_level(logging.WARNING, logger="ozy"):
        ozy.init(agent_host="127.0.0.1")
    assert "init failed" in caplog.text
    assert not ozy.tracer.enabled
    assert not ozy.statsd.enabled
