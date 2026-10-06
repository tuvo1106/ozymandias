"""The public ``Integration`` protocol, ``register_integration`` and ``patch_all``."""

from __future__ import annotations

import logging

import pytest

import ozy
from ozy.integrations import (
    Integration,
    get_integration,
    patch,
    patch_all,
    register_integration,
    unpatch_all,
)
from ozy.integrations._patching import PatchSet

from .conftest import FakeTraceAgent


class Recorder:
    """The smallest possible third-party integration: only the public protocol."""

    def __init__(self, name: str, *, available: bool = True, fail: bool = False) -> None:
        self.name = name
        self.available = available
        self.fail = fail
        self.patched = 0
        self.unpatched = 0

    def is_available(self) -> bool:
        return self.available

    def patch(self) -> None:
        if self.fail:
            raise RuntimeError("cannot patch")
        self.patched += 1

    def unpatch(self) -> None:
        if self.fail:
            raise RuntimeError("cannot unpatch")
        self.unpatched += 1


def test_the_protocol_is_structural() -> None:
    assert isinstance(Recorder("x"), Integration)
    assert not isinstance(object(), Integration)


def test_a_third_party_integration_uses_only_the_public_api(traced: FakeTraceAgent) -> None:
    # What a library integration does: wrap something, make spans with ozy.tracer.
    class Library:
        def work(self) -> str:
            return "done"

    class Mine:
        name = "mylib"
        original = Library.work

        def is_available(self) -> bool:
            return True

        def patch(self) -> None:
            original = Library.work

            def work(lib: Library) -> str:
                with ozy.tracer.trace("mylib.work", type="custom"):
                    return original(lib)

            Library.work = work  # type: ignore[method-assign,assignment]

        def unpatch(self) -> None:
            Library.work = Mine.original  # type: ignore[method-assign]

    mine = Mine()
    register_integration(mine)
    assert get_integration("mylib") is mine
    assert patch(["mylib"]) == ["mylib"]
    with ozy.tracer.trace("request"):
        assert Library().work() == "done"
    ozy.tracer.flush()
    assert {s["name"] for s in traced.spans()} == {"request", "mylib.work"}
    unpatch_all()
    assert Library.work is Mine.original


def test_patch_all_patches_every_available_registered_integration() -> None:
    ok, missing = Recorder("ok"), Recorder("missing", available=False)
    register_integration(ok)
    register_integration(missing)
    patched = patch_all()
    assert "ok" in patched
    assert "missing" not in patched
    assert (ok.patched, missing.patched) == (1, 0)
    assert "logging" in patched  # the built-ins load lazily by name


def test_a_failing_integration_is_logged_and_skipped(caplog: pytest.LogCaptureFixture) -> None:
    bad, good = Recorder("bad", fail=True), Recorder("good")
    register_integration(bad)
    register_integration(good)
    with caplog.at_level(logging.WARNING, logger="ozy"):
        assert patch(["bad", "good"]) == ["good"]  # never raises into the host
    assert "patching integration" in caplog.text
    with caplog.at_level(logging.WARNING, logger="ozy"):
        unpatch_all()
    assert good.unpatched == 1
    assert "unpatching" in caplog.text


def test_integration_objects_can_be_passed_directly() -> None:
    mine = Recorder("direct")
    assert patch([mine]) == ["direct"]
    assert get_integration("direct") is mine  # and are remembered for unpatch_all
    unpatch_all()
    assert mine.unpatched == 1


def test_register_replaces_by_name() -> None:
    a, b = Recorder("same"), Recorder("same")
    register_integration(a)
    register_integration(b)
    assert get_integration("same") is b


def test_unknown_names_are_none_and_a_broken_builtin_import_is_logged(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    assert get_integration("definitely-not-an-integration") is None
    import ozy.integrations as pkg

    monkeypatch.setitem(pkg._BUILTIN, "broken", ("ozy.integrations.no_such_module", "INTEGRATION"))
    with caplog.at_level(logging.WARNING, logger="ozy"):
        assert get_integration("broken") is None
    assert "failed to load" in caplog.text


def test_importing_the_package_imports_no_target_library() -> None:
    import subprocess
    import sys

    code = (
        "import sys, ozy, ozy.integrations;"
        "libs=('sqlalchemy','redis','httpx','arq','starlette','fastapi');"
        "bad=[m for m in libs if m in sys.modules];"
        "print(bad)"
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=True)
    assert out.stdout.strip() == "[]"


def test_importing_an_integration_module_without_its_library_is_safe() -> None:
    import subprocess
    import sys

    code = (
        "import sys\n"
        "sys.modules['sqlalchemy'] = None; sys.modules['redis'] = None\n"
        "sys.modules['httpx'] = None; sys.modules['arq'] = None; sys.modules['starlette'] = None\n"
        "import ozy\n"
        "from ozy.integrations import patch_all\n"
        "import ozy.integrations.sqlalchemy, ozy.integrations.redis, ozy.integrations.httpx\n"
        "import ozy.integrations.arq, ozy.integrations.asgi\n"
        "print(patch_all())\n"
    )
    out = subprocess.run([sys.executable, "-c", code], capture_output=True, text=True, check=False)
    assert out.returncode == 0, out.stderr
    assert out.stdout.strip() == "['logging']"  # only what needs no library


def test_patchset_restores_inherited_attributes_by_deleting_them() -> None:
    class Base:
        def f(self) -> str:
            return "base"

    class Child(Base):
        pass

    patches = PatchSet()
    patches.wrap(Child, "f", lambda original: lambda self: "patched " + original(self))
    assert Child().f() == "patched base"
    assert patches.active
    patches.undo()
    assert not patches.active
    assert "f" not in vars(Child)  # restored as inherited, not copied onto the subclass
    Base.f = lambda self: "later base"
    assert Child().f() == "later base"


def test_patchset_undo_survives_a_failed_restore(caplog: pytest.LogCaptureFixture) -> None:
    patches = PatchSet()
    patches._saved.append((object(), "nope", object()))  # setattr on an object() raises
    with caplog.at_level(logging.WARNING, logger="ozy"):
        patches.undo()
    assert "could not restore" in caplog.text


def test_unpatch_all_with_nothing_registered_is_fine() -> None:
    unpatch_all()
    assert patch([]) == []
