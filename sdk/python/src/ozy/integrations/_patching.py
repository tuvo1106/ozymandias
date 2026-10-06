"""Shared plumbing for integrations that wrap a library's methods.

``PatchSet`` records every attribute it replaces so ``unpatch()`` restores the library
exactly, including the case that is easy to get wrong: a method *inherited* from a base
class must be removed from the subclass on undo, not copied onto it (copying would pin
the base's current implementation and hide a later patch of the base).
"""

from __future__ import annotations

import logging
from collections.abc import Callable
from typing import Any

_log = logging.getLogger("ozy")

_MISSING = object()


def active_span() -> Any:
    """The tracer's active span, or ``None``. Client integrations only trace inside a trace."""
    from .. import tracer

    return tracer.current_span()


class PatchSet:
    """Wrap attributes on classes and undo every wrap later."""

    def __init__(self) -> None:
        """Create an empty set (nothing patched)."""
        self._saved: list[tuple[Any, str, Any]] = []

    @property
    def active(self) -> bool:
        """Whether anything is currently patched."""
        return bool(self._saved)

    def wrap(self, owner: Any, attr: str, factory: Callable[[Any], Any]) -> None:
        """Replace ``owner.attr`` with ``factory(original)``; remember how to undo it."""
        original = getattr(owner, attr)
        own = vars(owner).get(attr, _MISSING)
        replacement = factory(original)
        replacement.__ozy_wrapped__ = True
        self._saved.append((owner, attr, own))
        setattr(owner, attr, replacement)

    def undo(self) -> None:
        """Restore every attribute, newest first. Never raises."""
        while self._saved:
            owner, attr, own = self._saved.pop()
            try:
                if own is _MISSING:
                    delattr(owner, attr)
                else:
                    setattr(owner, attr, own)
            except Exception:
                _log.warning("ozy: could not restore %s.%s", owner, attr, exc_info=True)
