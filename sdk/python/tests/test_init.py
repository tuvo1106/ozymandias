"""The public entry points: ``ozy.init`` and the process-wide ``ozy.statsd``."""

from __future__ import annotations

import atexit
import gc
import os
from typing import Any

import pytest

import ozy
from ozy import StatsdClient, statsd
from ozy import statsd as statsd_again

from .conftest import FakeAgent


def test_statsd_is_one_stable_object() -> None:
    assert statsd is statsd_again is ozy.statsd
    ozy.init(agent_host="127.0.0.1")
    assert ozy.statsd is statsd


def test_init_from_env_end_to_end(monkeypatch: pytest.MonkeyPatch, agent: FakeAgent) -> None:
    monkeypatch.setenv("OZY_AGENT_HOST", "127.0.0.1")
    monkeypatch.setenv("OZY_STATSD_PORT", str(agent.port))
    monkeypatch.setenv("OZY_SERVICE", "env-svc")
    monkeypatch.setenv("OZY_TAGS", "team:core")
    ozy.init(env="dev")  # argument joins the env-provided settings
    assert statsd.enabled
    statsd.increment("hello", tags=["a:b"])
    statsd.flush()
    assert agent.recv() == "hello:1|c|#a:b,team:core,service:env-svc,env:dev"


def test_reinit_flushes_old_config_and_applies_new(agent: FakeAgent) -> None:
    other = FakeAgent()
    try:
        ozy.init(agent_host="127.0.0.1", statsd_port=agent.port, service="one")
        statsd.increment("m")
        ozy.init(agent_host="127.0.0.1", statsd_port=other.port, service="two")
        assert agent.recv() == "m:1|c|#service:one"
        statsd.increment("m")
        statsd.flush()
        assert other.recv() == "m:1|c|#service:two"
    finally:
        other.close()


def test_hooks_registered_once_and_only_when_enabled(monkeypatch: pytest.MonkeyPatch) -> None:
    exit_hooks: list[Any] = []
    fork_hooks: list[Any] = []
    monkeypatch.setattr(atexit, "register", lambda fn: exit_hooks.append(fn))
    monkeypatch.setattr(os, "register_at_fork", lambda **kw: fork_hooks.append(kw))

    client = StatsdClient()
    client.configure(ozy.Config())
    assert (exit_hooks, fork_hooks) == ([], [])
    client.configure(ozy.Config(agent_host="127.0.0.1"))
    client.configure(ozy.Config(agent_host="127.0.0.1"))
    assert len(exit_hooks) == 1
    assert len(fork_hooks) == 1
    assert set(fork_hooks[0]) == {"after_in_child"}

    # The at-exit hook closes the client.
    exit_hooks[0]()
    assert not client.enabled

    # Hooks hold only a weak reference: a discarded client is collectable
    # and its hooks become no-ops.
    del client
    gc.collect()
    exit_hooks[0]()
    fork_hooks[0]["after_in_child"]()


def test_version_is_exposed() -> None:
    assert ozy.__version__ == "0.1.0"
