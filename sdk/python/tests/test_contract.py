"""Hop A contract: every case in the shared goldens, over a real UDP socket.

``pkg/wire/testdata/statsd/sdk-cases.json`` is the byte-exact spec shared with
the Go parser tests and the Node SDK (docs/wire-protocol.md §A). If a case
changes there, this suite must change behaviour with it — never the reverse.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from ozy import StatsdClient

from .conftest import ClientFactory, FakeAgent

CASES_PATH = Path(__file__).resolve().parents[3] / "pkg/wire/testdata/statsd/sdk-cases.json"
SENTINEL = "contract.sentinel:1|c"
SPECIAL = {"nan": float("nan"), "inf": float("inf"), "-inf": float("-inf")}


def load_cases() -> list[dict[str, Any]]:
    with CASES_PATH.open(encoding="utf-8") as fh:
        cases: list[dict[str, Any]] = json.load(fh)["cases"]
    return cases


CASES = load_cases()


def test_goldens_are_nonempty() -> None:
    # Guards against a silently empty parametrization (e.g. a renamed key).
    assert len(CASES) >= 20


def call(client: StatsdClient, case: dict[str, Any]) -> None:
    method = getattr(client, case["call"])
    kwargs: dict[str, Any] = {}
    if "tags" in case:
        kwargs["tags"] = case["tags"]
    if "sample_rate" in case:
        kwargs["sample_rate"] = case["sample_rate"]
    if "value_special" in case:
        method(case["metric"], SPECIAL[case["value_special"]], **kwargs)
    elif "value" in case:
        method(case["metric"], case["value"], **kwargs)
    else:
        method(case["metric"], **kwargs)


@pytest.mark.parametrize("case", CASES, ids=[c["name"] for c in CASES])
def test_sdk_case(case: dict[str, Any], make_client: ClientFactory, agent: FakeAgent) -> None:
    init = dict(case["init"])
    if "tags" in init:
        init["tags"] = tuple(init["tags"])
    client = make_client(random=lambda: case.get("random", 0.0), **init)

    call(client, case)
    client.flush()

    if case["expect"] is None:
        # Prove absence without a sleep: whatever arrives first must be a
        # sentinel sent afterwards (the case's line would be in the same or
        # an earlier datagram). The sentinel is sent without global tags.
        bare = make_client()
        bare.increment("contract.sentinel")
        bare.flush()
        assert agent.recv() == SENTINEL
        assert client.stats().sent == 0
    else:
        assert agent.recv() == case["expect"]
        assert client.stats().sent == 1
