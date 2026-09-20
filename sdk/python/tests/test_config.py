"""Configuration layering: arguments > OZY_* environment > defaults."""

from __future__ import annotations

import pytest

from ozy._config import (
    DEFAULT_FLUSH_INTERVAL,
    DEFAULT_MAX_PAYLOAD,
    DEFAULT_STATSD_PORT,
    Config,
    resolve_config,
)


def test_defaults_are_disabled() -> None:
    cfg = resolve_config(environ={})
    assert cfg == Config()
    assert not cfg.enabled
    assert cfg.statsd_port == DEFAULT_STATSD_PORT
    assert cfg.max_payload == DEFAULT_MAX_PAYLOAD
    assert cfg.flush_interval == DEFAULT_FLUSH_INTERVAL


def test_reads_every_env_var() -> None:
    cfg = resolve_config(
        environ={
            "OZY_AGENT_HOST": "agent.local",
            "OZY_STATSD_PORT": "9125",
            "OZY_SERVICE": "svc",
            "OZY_ENV": "prod",
            "OZY_VERSION": "2.0",
            "OZY_TAGS": " team:core , ,region:eu ",
            "OZY_DEBUG": "TRUE",
        }
    )
    assert cfg == Config(
        agent_host="agent.local",
        statsd_port=9125,
        service="svc",
        env="prod",
        version="2.0",
        tags=("team:core", "region:eu"),
        debug=True,
    )
    assert cfg.enabled


def test_arguments_override_env() -> None:
    environ = {
        "OZY_AGENT_HOST": "from-env",
        "OZY_STATSD_PORT": "1",
        "OZY_SERVICE": "env-svc",
        "OZY_TAGS": "a:b",
        "OZY_DEBUG": "1",
    }
    cfg = resolve_config(
        agent_host="from-arg",
        statsd_port=2,
        service="arg-svc",
        tags=["c:d"],
        debug=False,
        environ=environ,
    )
    assert (cfg.agent_host, cfg.statsd_port, cfg.service, cfg.tags, cfg.debug) == (
        "from-arg",
        2,
        "arg-svc",
        ("c:d",),
        False,
    )


def test_empty_agent_host_argument_disables_even_with_env() -> None:
    cfg = resolve_config(agent_host="", environ={"OZY_AGENT_HOST": "agent"})
    assert not cfg.enabled


def test_blank_env_values_count_as_unset() -> None:
    cfg = resolve_config(environ={"OZY_AGENT_HOST": "  ", "OZY_SERVICE": ""})
    assert not cfg.enabled
    assert cfg.service is None


@pytest.mark.parametrize("raw", ["abc", "0", "70000", "-5"])
def test_bad_port_falls_back_to_default(raw: str) -> None:
    cfg = resolve_config(environ={"OZY_STATSD_PORT": raw})
    assert cfg.statsd_port == DEFAULT_STATSD_PORT


def test_single_string_tag_is_one_tag() -> None:
    assert resolve_config(tags="team:core", environ={}).tags == ("team:core",)


def test_nonpositive_limits_fall_back_to_defaults() -> None:
    cfg = resolve_config(max_payload=0, flush_interval=-1, environ={})
    assert cfg.max_payload == DEFAULT_MAX_PAYLOAD
    assert cfg.flush_interval == DEFAULT_FLUSH_INTERVAL


def test_global_tags_order_and_sanitation() -> None:
    cfg = Config(service="s|x", env="dev", version=None, tags=("a,b", ""))
    assert cfg.global_tags() == ("a_b", "service:s_x", "env:dev")


def test_reads_os_environ_by_default(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OZY_SERVICE", "from-os")
    assert resolve_config().service == "from-os"
