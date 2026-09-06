"""The wire envelopes of the WorkerDispatch stream.

Both directions are tagged unions carrying exactly one variant, encoded with
Go's ``encoding/json``. These are dataclasses over that JSON rather than
generated types: the server declares them as Go structs, not protobuf, so
there is nothing to generate from.

The one thing this module exists to get right is the base64 boundary. Go's
``[]byte`` marshals to a base64 **string**, so ``Dispatch.args`` and
``Dispatch.trace_ctx`` arrive as text that has to be decoded before it is JSON
again. A port that reads those fields as the payload hands a handler base64
text that parses as nothing.
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass, field
from typing import Any

__all__ = [
    "MAX_CHECKPOINT_MSG_CHARS",
    "MAX_HEARTBEAT_IDS_PER_FRAME",
    "MAX_IN_FLIGHT_CEILING",
    "MAX_JOB_NAMES_PER_OPEN",
    "Dispatch",
    "Goodbye",
    "ProtocolError",
    "Revoke",
    "ServerEnvelope",
    "SessionAccepted",
    "ack",
    "checkpoint_envelope",
    "complete",
    "decode_server_envelope",
    "encode",
    "fail",
    "heartbeat",
    "open_session",
]

# The server's own caps, from internal/server/jobsdispatcher/proto.go. Mirrored
# so the worker trims before sending rather than having a session closed with
# InvalidArgument for an envelope it could have shortened.
MAX_JOB_NAMES_PER_OPEN = 1024
MAX_HEARTBEAT_IDS_PER_FRAME = 4096
MAX_CHECKPOINT_MSG_CHARS = 256
MAX_IN_FLIGHT_CEILING = 256


class ProtocolError(Exception):
    """The server sent something this worker cannot read."""


@dataclass(frozen=True)
class SessionAccepted:
    """The server's first reply, carrying the lease parameters to target."""

    session_id: str
    lease_ttl_ms: int
    heartbeat_ms: int


@dataclass(frozen=True)
class Dispatch:
    """One claimed job handed to this worker.

    ``args`` is the decoded JSON object the typed handler receives — not the
    base64 the wire carried.
    """

    job_id: int
    job_name: str
    queue: str = ""
    args: dict[str, Any] = field(default_factory=dict)
    attempts: int = 0
    max_retries: int = 0
    timeout_ms: int = 0
    trace_ctx: bytes = b""


@dataclass(frozen=True)
class Revoke:
    """The server pulled a row back; the handler for it should be cancelled."""

    job_id: int
    reason: str = ""


@dataclass(frozen=True)
class Goodbye:
    """The server is closing the stream. Finish in-flight work and stop."""

    reason: str = ""


# Exactly one of these is set on any received envelope.
ServerEnvelope = SessionAccepted | Dispatch | Revoke | Goodbye


def decode_server_envelope(raw: bytes) -> ServerEnvelope:
    """Decode one server → worker envelope.

    Raises ProtocolError for an envelope carrying no variant this worker
    knows. Returning None instead would make an unrecognised envelope
    indistinguishable from a stream that has nothing to say, and the session
    would sit idle holding leases it is no longer being told about.
    """
    try:
        body = json.loads(raw)
    except ValueError as exc:
        raise ProtocolError(f"envelope is not JSON: {exc}") from exc
    if not isinstance(body, dict):
        raise ProtocolError(f"envelope is a {type(body).__name__}, not an object")

    if (sa := body.get("session_accepted")) is not None:
        return SessionAccepted(
            session_id=str(sa.get("session_id", "")),
            lease_ttl_ms=int(sa.get("lease_ttl_ms", 0)),
            heartbeat_ms=int(sa.get("heartbeat_ms", 0)),
        )
    if (d := body.get("dispatch")) is not None:
        return Dispatch(
            job_id=int(d["job_id"]),
            job_name=str(d.get("job_name", "")),
            queue=str(d.get("queue", "")),
            args=_decode_args(d.get("args")),
            attempts=int(d.get("attempts", 0)),
            max_retries=int(d.get("max_retries", 0)),
            timeout_ms=int(d.get("timeout_ms", 0)),
            trace_ctx=_decode_bytes(d.get("trace_ctx")),
        )
    if (r := body.get("revoke")) is not None:
        return Revoke(job_id=int(r["job_id"]), reason=str(r.get("reason", "")))
    if (g := body.get("goodbye")) is not None:
        return Goodbye(reason=str(g.get("reason", "")))

    raise ProtocolError(f"envelope carries no known variant: {sorted(body)}")


def _decode_bytes(value: object) -> bytes:
    """Decode a Go []byte field.

    Absent and empty are both b"": Go omits the key for a nil slice, and an
    empty slice encodes as "".
    """
    if value is None or value == "":
        return b""
    if not isinstance(value, str):
        raise ProtocolError(
            f"expected a base64 string for a bytes field, got {type(value).__name__}"
        )
    try:
        return base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as exc:
        raise ProtocolError(f"bytes field is not base64: {exc}") from exc


def _decode_args(value: object) -> dict[str, Any]:
    """Decode Dispatch.args: base64 on the wire, JSON inside.

    A job declaring no args has none on the wire, which is an empty mapping
    here rather than an error — the generated Args dataclass for such a job
    has no fields to fill.
    """
    raw = _decode_bytes(value)
    if not raw:
        return {}
    try:
        decoded = json.loads(raw)
    except ValueError as exc:
        raise ProtocolError(f"args are not JSON once base64-decoded: {exc}") from exc
    if not isinstance(decoded, dict):
        raise ProtocolError(f"args decoded to a {type(decoded).__name__}, not an object")
    return decoded


def encode(envelope: dict[str, Any]) -> bytes:
    """Encode one worker → server envelope.

    Separators without spaces, matching what Go's json.Marshal writes. Nothing
    on the server compares these bytes, but two encoders that differ only in
    whitespace make a captured frame hard to compare against a Go one.
    """
    return json.dumps(envelope, separators=(",", ":")).encode()


def open_session(
    queue: str, job_names: list[str], max_in_flight: int, pod_id: str, version: str
) -> dict[str, Any]:
    """The first envelope. Job names are trimmed to the server's cap."""
    return {
        "open": {
            "queue": queue,
            "job_names": job_names[:MAX_JOB_NAMES_PER_OPEN],
            "max_in_flight": max_in_flight,
            "pod_id": pod_id,
            "version": version,
        }
    }


def heartbeat(job_ids: list[int]) -> dict[str, Any]:
    """A batched lease bump for everything still in flight."""
    return {"heartbeat": {"job_ids": job_ids[:MAX_HEARTBEAT_IDS_PER_FRAME]}}


def checkpoint_envelope(job_id: int, pct: int, msg: str) -> dict[str, Any]:
    """Progress for one job. Pct is clamped and msg truncated as the server does."""
    return {
        "checkpoint": {
            "job_id": job_id,
            "pct": max(0, min(100, pct)),
            "msg": msg[:MAX_CHECKPOINT_MSG_CHARS],
        }
    }


def ack(job_id: int) -> dict[str, Any]:
    """Sent before the handler runs; without it the server revokes the row."""
    return {"ack": {"job_id": job_id}}


def complete(job_id: int) -> dict[str, Any]:
    return {"complete": {"job_id": job_id}}


def fail(job_id: int, error: str, *, retry: bool) -> dict[str, Any]:
    """Terminal failure. retry=False sends the row straight to the dead-letter
    queue whatever attempts remain."""
    return {"fail": {"job_id": job_id, "error": error, "retry": retry}}
