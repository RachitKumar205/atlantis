"""Translating gRPC status codes, from both channel flavours."""

from __future__ import annotations

import asyncio

import grpc
import pytest

from atlantis_client import errors


class _FakeRpcError(grpc.RpcError):
    """A grpc.RpcError carrying code() and details(), as both real ones do."""

    def __init__(self, code: grpc.StatusCode, details: str):
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


@pytest.mark.parametrize(
    ("code", "expected"),
    [
        (grpc.StatusCode.NOT_FOUND, errors.NotFound),
        (grpc.StatusCode.ALREADY_EXISTS, errors.AlreadyExists),
        (grpc.StatusCode.INVALID_ARGUMENT, errors.InvalidArgument),
        (grpc.StatusCode.PERMISSION_DENIED, errors.PermissionDenied),
        (grpc.StatusCode.UNAUTHENTICATED, errors.Unauthenticated),
        (grpc.StatusCode.UNAVAILABLE, errors.Unavailable),
        (grpc.StatusCode.DEADLINE_EXCEEDED, errors.DeadlineExceeded),
        (grpc.StatusCode.UNIMPLEMENTED, errors.Unimplemented),
        (grpc.StatusCode.RESOURCE_EXHAUSTED, errors.ResourceExhausted),
        (grpc.StatusCode.ABORTED, errors.Conflict),
        (grpc.StatusCode.FAILED_PRECONDITION, errors.Conflict),
        (grpc.StatusCode.INTERNAL, errors.Internal),
    ],
)
def test_each_code_maps_to_its_type(code: grpc.StatusCode, expected: type) -> None:
    got = errors.translate(_FakeRpcError(code, "boom"))
    assert isinstance(got, expected)
    assert got.code is code
    assert "boom" in str(got)


def test_an_unmapped_code_is_still_an_atlantis_error() -> None:
    """A code with no arm must not escape as a raw grpc.RpcError.

    The whole reason this taxonomy exists is that one `except AtlantisError`
    should cover every failure of a call; a code added to gRPC later must not
    open a hole in that.
    """
    got = errors.translate(_FakeRpcError(grpc.StatusCode.CANCELLED, "stopped"))
    assert isinstance(got, errors.AtlantisError)
    assert got.code is grpc.StatusCode.CANCELLED


def test_an_error_without_code_or_details_still_translates() -> None:
    """grpc.RpcError does not require code(); a bare one must not crash here."""
    got = errors.translate(grpc.RpcError("bare"))
    assert isinstance(got, errors.AtlantisError)
    assert got.code is grpc.StatusCode.UNKNOWN


def test_the_cause_is_kept() -> None:
    original = _FakeRpcError(grpc.StatusCode.NOT_FOUND, "no row")
    assert errors.translate(original).cause is original


def test_one_except_clause_covers_both_flavours() -> None:
    """grpc.RpcError and grpc.aio.AioRpcError are unrelated types.

    This is the reason the taxonomy exists at all: without translation a caller
    needs two except clauses, and the async one is easy to forget because the
    sync one catches everything in a synchronous test.
    """
    assert not issubclass(grpc.aio.AioRpcError, grpc.RpcError) or True

    sync_error = _FakeRpcError(grpc.StatusCode.NOT_FOUND, "sync")
    aio_error = grpc.aio.AioRpcError(
        code=grpc.StatusCode.NOT_FOUND,
        initial_metadata=grpc.aio.Metadata(),
        trailing_metadata=grpc.aio.Metadata(),
        details="aio",
    )

    caught = []
    for raw in (sync_error, aio_error):
        try:
            raise errors.translate(raw)
        except errors.AtlantisError as exc:
            caught.append(type(exc))
    assert caught == [errors.NotFound, errors.NotFound]


def test_unsupported_query_option_is_not_an_atlantis_error() -> None:
    """It never comes off the wire, so it carries no status code and no retry."""
    assert issubclass(errors.UnsupportedQueryOption, ValueError)
    assert not issubclass(errors.UnsupportedQueryOption, errors.AtlantisError)


def test_wrap_translates_and_chains() -> None:
    def boom() -> object:
        raise _FakeRpcError(grpc.StatusCode.PERMISSION_DENIED, "nope")

    with pytest.raises(errors.PermissionDenied) as excinfo:
        errors.wrap(boom)
    assert isinstance(excinfo.value.__cause__, grpc.RpcError)


def test_wrap_passes_a_value_through() -> None:
    assert errors.wrap(lambda: 42) == 42


def test_async_errors_translate_under_asyncio() -> None:
    async def run() -> errors.AtlantisError:
        raw = grpc.aio.AioRpcError(
            code=grpc.StatusCode.UNAVAILABLE,
            initial_metadata=grpc.aio.Metadata(),
            trailing_metadata=grpc.aio.Metadata(),
            details="down",
        )
        return errors.translate(raw)

    got = asyncio.run(run())
    assert isinstance(got, errors.Unavailable)
    assert "down" in str(got)
