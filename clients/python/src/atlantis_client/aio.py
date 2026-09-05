"""The async flavour of :mod:`atlantis_client.transport`.

Same credentials, same options, ``grpc.aio`` channels. Kept in its own module
so a synchronous caller never imports the asyncio machinery, and so the two
``connect`` names cannot be confused at a call site.
"""

from __future__ import annotations

import grpc

from .transport import (
    CHANNEL_OPTIONS,
    TransportError,
    channel_credentials,
    channel_credentials_from_store,
)

__all__ = ["connect", "connect_from_store"]


def connect(target: str, *, options: tuple[tuple[str, int], ...] | None = None) -> grpc.aio.Channel:
    """Open an async channel to target using the env-var contract."""
    return grpc.aio.secure_channel(
        target,
        channel_credentials(),
        options=list(options if options is not None else CHANNEL_OPTIONS),
    )


def connect_from_store(
    org: str,
    caller: str,
    *,
    target: str | None = None,
    options: tuple[tuple[str, int], ...] | None = None,
) -> grpc.aio.Channel:
    """Open an async channel using the credential store."""
    creds, endpoint = channel_credentials_from_store(org, caller)
    address = target or endpoint
    if not address:
        raise TransportError(
            f"no endpoint recorded for {org}/{caller} and none given. "
            "Pass target=, or re-run `tide login` to record one."
        )
    return grpc.aio.secure_channel(
        address,
        creds,
        options=list(options if options is not None else CHANNEL_OPTIONS),
    )
