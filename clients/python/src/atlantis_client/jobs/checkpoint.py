"""Reporting handler progress.

A handler calls :func:`checkpoint` to flush progress to atlantis without
waiting for the next heartbeat. It reaches the worker through a ContextVar
rather than an argument, so a handler signature stays ``(args) -> None`` and
helper functions several frames deep can report without threading a parameter
through every one.

The ContextVar is set **inside** the worker thread that runs the handler. A
ContextVar set on the main thread is not visible to a pool thread — each
thread starts from an empty context — so setting it where the worker submits
the job would leave every ``checkpoint()`` call silently doing nothing.
"""

from __future__ import annotations

import contextvars
from typing import Protocol

__all__ = ["Checkpointer", "checkpoint", "current_checkpointer", "use_checkpointer"]


class Checkpointer(Protocol):
    """What the worker installs for the job currently running on this thread."""

    def report(self, pct: int, msg: str) -> None: ...


_current: contextvars.ContextVar[Checkpointer | None] = contextvars.ContextVar(
    "atlantis_checkpointer", default=None
)


def current_checkpointer() -> Checkpointer | None:
    """The checkpointer for the job on this thread, or None outside one."""
    return _current.get()


def checkpoint(pct: int, msg: str = "") -> None:
    """Report progress for the job running on this thread.

    Does nothing when called outside a handler, so a function that reports
    progress stays callable from a unit test with no worker attached. The
    alternative — raising — would make every such helper untestable without
    building a session.

    ``pct`` is clamped to 0..100 and ``msg`` truncated by the encoder; both
    limits are the server's.
    """
    sink = _current.get()
    if sink is None:
        return
    sink.report(pct, msg)


class use_checkpointer:
    """Install a checkpointer for the duration of one handler call.

    A context manager rather than a bare ``set``, so the token is always reset
    — a worker thread is reused for the next job, and a leaked checkpointer
    would send the next job's progress against the previous job's id.
    """

    def __init__(self, sink: Checkpointer) -> None:
        self._sink = sink
        self._token: contextvars.Token[Checkpointer | None] | None = None

    def __enter__(self) -> None:
        self._token = _current.set(self._sink)

    def __exit__(self, *exc: object) -> None:
        if self._token is not None:
            _current.reset(self._token)
            self._token = None
