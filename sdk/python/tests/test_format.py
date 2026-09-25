"""Unit tests for the pure formatting helpers (the contract suite covers the goldens)."""

from __future__ import annotations

import math
from decimal import Decimal
from fractions import Fraction

import pytest

from ozy._format import format_line, format_number, sanitize_name, sanitize_tag

from .conftest import ClientFactory, FakeAgent


@pytest.mark.parametrize(
    ("value", "expected"),
    [
        (0, "0"),
        (-0.0, "0"),
        (True, "1"),
        (3, "3"),
        (3.0, "3"),
        (-2.0, "-2"),
        (0.1, "0.1"),
        (1e-7, "1e-07"),
        (1e16, "1e+16"),
        (123456789012345.0, "123456789012345"),
        (Decimal("2.5"), "2.5"),
        (Fraction(1, 4), "0.25"),
    ],
)
def test_format_number(value: object, expected: str) -> None:
    assert format_number(value) == expected


@pytest.mark.parametrize("value", [math.nan, math.inf, -math.inf])
def test_format_number_rejects_non_finite(value: float) -> None:
    assert format_number(value) is None


def test_format_number_rejects_strings() -> None:
    with pytest.raises(TypeError):
        format_number("3")


def test_format_number_roundtrips_random_floats() -> None:
    import random

    rng = random.Random(1234)
    for _ in range(2000):
        v = rng.uniform(-1e6, 1e6) * 10 ** rng.randint(-12, 12)
        text = format_number(v)
        assert text is not None
        assert float(text) == v
        assert not text.endswith(".0")


def test_sanitize_name_and_tag() -> None:
    assert sanitize_name("a|b:c,d\ne") == "a_b_c_d_e"
    assert sanitize_tag("k:v|w,x\ny") == "k:v_w_x_y"


def test_format_line_order_and_omissions() -> None:
    assert format_line("m", "1", "c", 1.0, ()) == "m:1|c"
    assert format_line("m", "1", "c", 0.25, ["a:b", "c"]) == "m:1|c|@0.25|#a:b,c"
    assert format_line("m", "1", "c", 2.0, ["a"]) == "m:1|c|#a"


def test_set_member_comma_becomes_underscore(make_client: ClientFactory, agent: FakeAgent) -> None:
    # wire-protocol §A: '|', ',' and newline become '_' in set members.
    client = make_client()
    client.set("users.unique", "a,b")
    client.flush()
    assert agent.recv() == "users.unique:a_b|s"
