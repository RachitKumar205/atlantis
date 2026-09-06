"""What a worker knows how to run.

A registry maps a job's canonical id — ``<namespace>.<JobName>`` — to the
callable that handles it. The generated ``register_<Job>`` helpers fill it, so
a caller writes handlers and never touches the ids directly.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping
from typing import Any

__all__ = ["Handler", "Registry"]

# A handler takes the decoded args mapping and returns nothing. It signals
# failure by raising: a return value would need a convention for "failed but
# retry" that an exception already carries.
#
# Imported at run time rather than under TYPE_CHECKING, because this alias is
# a value a caller can annotate with — under TYPE_CHECKING it would be a name
# that exists only to a type checker and raises NameError anywhere else.
Handler = Callable[[Mapping[str, Any]], None]


class Registry:
    """The job ids this worker declares at Open, and their handlers.

    Registering the same id twice raises rather than replacing. Two handlers
    for one job is a wiring mistake, and silently keeping the last one makes
    which handler runs depend on import order.
    """

    def __init__(self) -> None:
        self._handlers: dict[str, Callable[[Mapping[str, Any]], None]] = {}

    def register(self, job_name: str, handler: Callable[[Mapping[str, Any]], None]) -> None:
        if job_name in self._handlers:
            raise ValueError(
                f"{job_name} already has a handler. Registering twice makes which one "
                f"runs depend on import order."
            )
        self._handlers[job_name] = handler

    def get(self, job_name: str) -> Callable[[Mapping[str, Any]], None] | None:
        return self._handlers.get(job_name)

    def job_names(self) -> list[str]:
        """The ids to declare at Open, sorted so the envelope is stable."""
        return sorted(self._handlers)

    def __len__(self) -> int:
        return len(self._handlers)
