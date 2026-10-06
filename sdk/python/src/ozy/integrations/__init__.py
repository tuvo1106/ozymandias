"""Integrations: small adapters from a library or framework to the public tracer API.

Each integration is built **only** on what ``ozy.tracer`` (and ``ozy.statsd``) expose,
so a third party can write one with exactly the same power; see "Writing your own
integration" in ``docs/sdk/python.md``. Importing this package imports nothing it
targets: the ASGI middleware speaks the ASGI spec rather than Starlette's classes, and
every other integration imports its library lazily inside ``is_available()``/``patch()``
and does nothing when it is absent. That is how the SDK keeps zero dependencies.

The contract (:class:`Integration`): a ``name``, ``is_available()`` (can I patch? no
side effects), ``patch()`` and ``unpatch()``, both idempotent and neither allowed to
raise into the host. :func:`patch` and :func:`patch_all` enforce that last part: an
integration that fails is logged and skipped, because instrumentation must not stop an
app booting.
"""

from __future__ import annotations

import importlib
import logging
from collections.abc import Iterable
from typing import Protocol, runtime_checkable

__all__ = [
    "Integration",
    "get_integration",
    "patch",
    "patch_all",
    "register_integration",
    "unpatch_all",
]

_log = logging.getLogger("ozy")


@runtime_checkable
class Integration(Protocol):
    """What an integration must provide.

    Attributes:
        name: Short unique name, what ``init(integrations=[...])`` refers to.
    """

    name: str

    def is_available(self) -> bool:
        """Whether the target library is importable. Must not patch or raise."""
        ...

    def patch(self) -> None:
        """Start instrumenting. Idempotent: patching twice must not double-wrap."""
        ...

    def unpatch(self) -> None:
        """Restore everything ``patch()`` changed. Idempotent."""
        ...


_BUILTIN = {
    "asgi": ("ozy.integrations.asgi", "INTEGRATION"),
    "sqlalchemy": ("ozy.integrations.sqlalchemy", "INTEGRATION"),
    "redis": ("ozy.integrations.redis", "INTEGRATION"),
    "httpx": ("ozy.integrations.httpx", "INTEGRATION"),
    "arq": ("ozy.integrations.arq", "INTEGRATION"),
    "logging": ("ozy.integrations.logging", "INTEGRATION"),
}
_registry: dict[str, Integration] = {}


def register_integration(integration: Integration) -> None:
    """Make an integration known to :func:`patch_all` and ``init(integrations=[...])``.

    The built-ins are registered lazily by name; this is the same door for your own.
    Registering a name twice replaces the earlier one.
    """
    _registry[integration.name] = integration


def get_integration(name: str) -> Integration | None:
    """The integration registered under ``name`` (built-ins load on first use), or ``None``."""
    found = _registry.get(name)
    if found is not None:
        return found
    target = _BUILTIN.get(name)
    if target is None:
        return None
    try:
        loaded: Integration = getattr(importlib.import_module(target[0]), target[1])
    except Exception:
        _log.warning("ozy: integration %r failed to load", name, exc_info=True)
        return None
    _registry[name] = loaded
    return loaded


def patch(integrations: Iterable[str | Integration]) -> list[str]:
    """Patch the given integrations (names or objects); returns the names that were patched.

    One that is unavailable (library not installed) is skipped silently; one that
    raises is logged and skipped. Never raises.
    """
    patched: list[str] = []
    for item in integrations:
        try:
            integration = get_integration(item) if isinstance(item, str) else item
            if integration is None:
                _log.warning("ozy: unknown integration %r", item)
                continue
            if isinstance(item, Integration) and item.name not in _registry:
                register_integration(item)
            if not integration.is_available():
                continue
            integration.patch()
            patched.append(integration.name)
        except Exception:
            _log.warning("ozy: patching integration %r failed", item, exc_info=True)
    return patched


def patch_all() -> list[str]:
    """Patch every built-in and registered integration whose library is installed."""
    names = list(_BUILTIN) + [n for n in _registry if n not in _BUILTIN]
    return patch(names)


def unpatch_all() -> None:
    """Undo every integration that is currently registered. Never raises."""
    for integration in list(_registry.values()):
        try:
            integration.unpatch()
        except Exception:
            _log.warning("ozy: unpatching %r failed", integration.name, exc_info=True)
