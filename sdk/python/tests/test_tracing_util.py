"""Shared vectors and pure helpers: sampling, the path normalizer, propagation parsing.

``pkg/wire/testdata/traces/*.json`` are the byte-exact spec shared with Go
(``wire.SampleKeep``, ``wire.NormalizePath``) and the Node SDK (docs/wire-protocol.md §B).
"""

from __future__ import annotations

import json
import random
from pathlib import Path
from typing import Any

import pytest

from ozy import Context, tracer
from ozy._tracing import normalize_path, parse_propagation, sample_keep

TRACES = Path(__file__).resolve().parents[3] / "pkg/wire/testdata/traces"


def load(name: str) -> list[dict[str, Any]]:
    with (TRACES / name).open(encoding="utf-8") as fh:
        cases: list[dict[str, Any]] = json.load(fh)["cases"]
    return cases


SAMPLING = load("sampling.json")
PATHS = load("normalize-path.json")


def test_vector_files_are_nonempty() -> None:
    # Guards against a silently empty parametrization (a renamed key).
    assert len(SAMPLING) > 100
    assert len(PATHS) >= 20
    assert {c["keep"] for c in SAMPLING} == {True, False}


def test_every_sampling_vector_agrees() -> None:
    wrong = [c for c in SAMPLING if sample_keep(c["trace_id"], c["rate"]) != c["keep"]]
    assert wrong == []


@pytest.mark.parametrize("case", PATHS, ids=lambda c: c["in"][:40] or "<empty>")
def test_path_normalizer_vectors(case: dict[str, str]) -> None:
    assert normalize_path(case["in"]) == case["out"]


def test_sampling_edges() -> None:
    tid = "0123456789abcdef0123456789abcdef"
    assert sample_keep(tid, 0) is False
    assert sample_keep(tid, -1) is False
    assert sample_keep(tid, float("nan")) is False
    assert sample_keep(tid, 1) is True
    assert sample_keep(tid, 7) is True
    assert sample_keep("short", 0.5) is False
    assert sample_keep("z" * 32, 0.5) is False


def test_sampling_rate_is_roughly_honoured() -> None:
    rng = random.Random(7)
    ids = [rng.getrandbits(128).to_bytes(16, "big").hex() for _ in range(20000)]
    kept = sum(sample_keep(t, 0.1) for t in ids)
    assert 1700 < kept < 2300


def test_path_normalizer_is_ascii_only_for_digits() -> None:
    # Arabic-Indic digits are str.isdigit() but not what the Go twin treats as digits.
    assert normalize_path("/x/١٢٣") == "/x/١٢٣"
    assert (
        normalize_path("/x/\n") == "/x/\n"
    )  # a trailing newline must not satisfy a $-anchored regex


def test_path_without_leading_slash_gets_one() -> None:
    assert normalize_path("a/b") == "/a/b"
    assert normalize_path("?x=1") == "/"


GOOD = "0123456789abcdef0123456789abcdef"
SPAN = "0123456789abcdef"


def test_parse_propagation_accepts_valid_and_lowers_case() -> None:
    assert parse_propagation(GOOD.upper(), SPAN.upper(), "2") == Context(GOOD, SPAN, 2)
    assert parse_propagation(GOOD, SPAN) == Context(GOOD, SPAN, 1)
    assert parse_propagation(GOOD, SPAN, "") == Context(GOOD, SPAN, 1)
    assert parse_propagation(f" {GOOD} ", SPAN, " -1 ") == Context(GOOD, SPAN, -1)
    assert parse_propagation(GOOD.encode(), SPAN.encode(), b"0") == Context(GOOD, SPAN, 0)
    assert parse_propagation(GOOD, SPAN, 1) == Context(GOOD, SPAN, 1)


@pytest.mark.parametrize(
    ("tid", "pid", "prio"),
    [
        (GOOD[:-1], SPAN, "1"),
        (GOOD + "0", SPAN, "1"),
        ("0" * 32, SPAN, "1"),
        (GOOD, "0" * 16, "1"),
        (GOOD, SPAN[:-1], "1"),
        ("g" * 32, SPAN, "1"),
        (GOOD, SPAN, "3"),
        (GOOD, SPAN, "-2"),
        (GOOD, SPAN, "one"),
        (GOOD, SPAN, "1.0"),
        (GOOD, SPAN, True),
        (None, SPAN, "1"),
        (GOOD, None, "1"),
        (b"\xff" * 32, SPAN, "1"),
        (GOOD, b"\xff" * 16, "1"),
        (GOOD, SPAN, b"\xff"),
        (["x"], SPAN, "1"),
        (GOOD, SPAN, 1.5),
    ],
)
def test_malformed_propagation_yields_no_context(tid: Any, pid: Any, prio: Any) -> None:
    assert parse_propagation(tid, pid, prio) is None


def test_garbage_carriers_never_raise_and_yield_none() -> None:
    rng = random.Random(1)
    junk: list[Any] = [None, 1, "x", b"y", [], (), {}, {"x-ozy-trace-id": object()}]
    for _ in range(500):
        junk.append(
            {
                rng.choice(["x-ozy-trace-id", "X-Ozy-Trace-Id", "trace_id", "z"]): rng.choice(
                    [GOOD, "", None, 5, b"\x00", "\u0000" * 3, GOOD[:5], object()]
                ),
                rng.choice(["x-ozy-parent-id", "parent_id"]): rng.choice([SPAN, "", None, 3.5]),
                "sampling_priority": rng.choice(["9", None, "-1", object()]),
            }
        )
    for carrier in junk:
        result = tracer.extract(carrier)
        assert result is None or isinstance(result, Context)
