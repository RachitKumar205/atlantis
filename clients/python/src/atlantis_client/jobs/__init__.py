"""Running atlantis jobs in Python.

atlantis claims rows and pushes them down a bidi stream; this package acks,
runs the handler, and reports the outcome. Nothing here talks to Postgres.

A worker is a registry, a channel and a queue::

    from atlantis_client import connect
    from atlantis_client.jobs import DispatchedWorker, Registry, WorkerConfig
    from atlantis.library.v1.jobs import register_reindex_author

    registry = Registry()
    register_reindex_author(registry, handle_reindex)

    with connect("atlantis.example.com:8443") as channel:
        DispatchedWorker(channel, registry, WorkerConfig(queue="maintenance")).run()

A handler reports progress with :func:`checkpoint`, which does nothing when
called outside one — so a function that reports stays callable from a test
with no worker attached.
"""

from __future__ import annotations

from ._envelopes import Dispatch, Goodbye, ProtocolError, Revoke, SessionAccepted
from .checkpoint import Checkpointer, checkpoint, current_checkpointer
from .registry import Handler, Registry
from .worker import FRAMED_METHOD, DispatchedWorker, WorkerConfig

__all__ = [
    "FRAMED_METHOD",
    "Checkpointer",
    "Dispatch",
    "DispatchedWorker",
    "Goodbye",
    "Handler",
    "ProtocolError",
    "Registry",
    "Revoke",
    "SessionAccepted",
    "WorkerConfig",
    "checkpoint",
    "current_checkpointer",
]
