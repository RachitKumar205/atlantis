"""The exception a caller catches, and the translation from gRPC status codes.

Go has no error taxonomy here and does not need one: a caller reads
``status.Code(err)``. Python needs one because ``grpc.RpcError`` and
``grpc.aio.AioRpcError`` are unrelated types, so no single ``except`` clause
covers both channel flavours without it.

Every raised error keeps the originating ``grpc`` exception on ``.cause`` and
its status code on ``.code``.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

import grpc

if TYPE_CHECKING:
    from collections.abc import Callable

__all__ = [
    "AlreadyExists",
    "AtlantisError",
    "Conflict",
    "DeadlineExceeded",
    "Internal",
    "InvalidArgument",
    "NotFound",
    "PermissionDenied",
    "ResourceExhausted",
    "Unauthenticated",
    "Unavailable",
    "Unimplemented",
    "UnsupportedQueryOption",
    "translate",
    "wrap",
]


class AtlantisError(Exception):
    """Base for every error raised by a generated client.

    ``code`` is the gRPC status code and ``cause`` the underlying
    ``grpc.RpcError`` or ``grpc.aio.AioRpcError``.
    """

    def __init__(self, message: str, code: grpc.StatusCode, cause: BaseException | None = None):
        super().__init__(message)
        self.code = code
        self.cause = cause


class NotFound(AtlantisError):
    """No row matched the primary key."""


class AlreadyExists(AtlantisError):
    """A unique constraint rejected the write."""


class InvalidArgument(AtlantisError):
    """The request named something the schema does not have, or a bad value."""


class PermissionDenied(AtlantisError):
    """The caller's certificate does not carry the capability for this call."""


class Unauthenticated(AtlantisError):
    """No usable client certificate, or no tenant on a partitioned entity."""


class Unavailable(AtlantisError):
    """The server could not be reached, or refused the connection."""


class DeadlineExceeded(AtlantisError):
    """The call outran its deadline."""


class Unimplemented(AtlantisError):
    """The server does not serve this call or this option."""


class ResourceExhausted(AtlantisError):
    """A limit was hit — message size, rate, or connection count."""


class Conflict(AtlantisError):
    """The write lost a concurrency check and can be retried."""


class Internal(AtlantisError):
    """The server failed in a way it does not attribute to the request."""


class UnsupportedQueryOption(ValueError):
    """A query option this client cannot send.

    A ValueError and not an AtlantisError: it never comes off the wire. The
    request is refused before a connection is used, so there is no status code
    to carry and nothing to retry.
    """


_BY_CODE: dict[grpc.StatusCode, type[AtlantisError]] = {
    grpc.StatusCode.NOT_FOUND: NotFound,
    grpc.StatusCode.ALREADY_EXISTS: AlreadyExists,
    grpc.StatusCode.INVALID_ARGUMENT: InvalidArgument,
    grpc.StatusCode.PERMISSION_DENIED: PermissionDenied,
    grpc.StatusCode.UNAUTHENTICATED: Unauthenticated,
    grpc.StatusCode.UNAVAILABLE: Unavailable,
    grpc.StatusCode.DEADLINE_EXCEEDED: DeadlineExceeded,
    grpc.StatusCode.UNIMPLEMENTED: Unimplemented,
    grpc.StatusCode.RESOURCE_EXHAUSTED: ResourceExhausted,
    grpc.StatusCode.ABORTED: Conflict,
    grpc.StatusCode.FAILED_PRECONDITION: Conflict,
    grpc.StatusCode.INTERNAL: Internal,
    grpc.StatusCode.DATA_LOSS: Internal,
    grpc.StatusCode.UNKNOWN: Internal,
}


def translate(exc: grpc.RpcError) -> AtlantisError:
    """Return the AtlantisError for a gRPC error from either channel flavour.

    ``code()`` and ``details()`` are on the ``grpc.Call`` half of both
    exception types, and a raised error that lacks them — which the API does
    not forbid — still has to produce something a caller can catch.
    """
    code = grpc.StatusCode.UNKNOWN
    details = str(exc)
    getter = getattr(exc, "code", None)
    if callable(getter):
        got = getter()
        if isinstance(got, grpc.StatusCode):
            code = got
    detail_getter = getattr(exc, "details", None)
    if callable(detail_getter):
        got_details = detail_getter()
        if got_details:
            details = str(got_details)

    return _BY_CODE.get(code, AtlantisError)(details, code, exc)


def wrap(fn: Callable[[], object]) -> object:
    """Call fn, translating any gRPC error. Used by the sync generated client."""
    try:
        return fn()
    except grpc.RpcError as exc:
        raise translate(exc) from exc
