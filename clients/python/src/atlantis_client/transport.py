"""Channel credentials and channels, in both flavours.

Two ways to supply TLS material, matching the Go SDK:

  - the env-var contract, all three required —
    ``ATL_TLS_CERT``, ``ATL_TLS_KEY``, ``ATL_TLS_CA``
  - the credential store ``tide login`` writes, addressed by organisation and
    caller

There is no insecure mode. atlantis reads the caller's identity from the
client certificate's CN, and the allowlist, the caller-to-certificate binding
and the capability grants all key off it — a connection with no client
certificate has nothing for authorization to be about.
"""

from __future__ import annotations

import os
from pathlib import Path

import grpc

from . import credentials as credstore

__all__ = [
    "CHANNEL_OPTIONS",
    "MAX_MESSAGE_BYTES",
    "TransportError",
    "channel_credentials",
    "channel_credentials_from_store",
    "connect",
    "connect_from_store",
]


class TransportError(Exception):
    """TLS material is missing or unusable."""


# 64 MiB, matching the Go SDK. grpc's own default is 4 MiB, which one page of a
# bulk Query carrying large jsonb or bytea columns overflows; the call then
# fails with "Received message larger than max" and no page size that a caller
# can set fixes it, because the limit is on the wire message, not the row count.
MAX_MESSAGE_BYTES = 64 << 20

CHANNEL_OPTIONS: tuple[tuple[str, int], ...] = (
    ("grpc.max_receive_message_length", MAX_MESSAGE_BYTES),
    ("grpc.max_send_message_length", MAX_MESSAGE_BYTES),
)

_ENV_CERT = "ATL_TLS_CERT"
_ENV_KEY = "ATL_TLS_KEY"
_ENV_CA = "ATL_TLS_CA"


def channel_credentials() -> grpc.ChannelCredentials:
    """Build channel credentials from the env-var contract.

    All three variables are required. Falling back to an insecure channel when
    ``ATL_TLS_CERT`` is unset would replace a legible configuration error with
    a handshake failure one layer below where the cause is readable.

    Unlike the Go SDK this cannot pin a minimum TLS version:
    ``grpc.ssl_channel_credentials`` exposes no such argument. The server's own
    floor still refuses anything below TLS 1.3, so the connection fails closed
    — but it fails at the server rather than before the first byte.
    """
    paths = {name: os.environ.get(name, "") for name in (_ENV_CERT, _ENV_KEY, _ENV_CA)}
    missing = [name for name, value in paths.items() if not value]
    if missing:
        raise TransportError(
            f"{', '.join(missing)} not set. atlantis authenticates every caller by "
            f"client certificate and accepts no plaintext connection, so all three "
            f"of {_ENV_CERT}, {_ENV_KEY} and {_ENV_CA} are required."
        )
    return grpc.ssl_channel_credentials(
        root_certificates=_read(paths[_ENV_CA], "CA bundle"),
        private_key=_read(paths[_ENV_KEY], "client key"),
        certificate_chain=_read(paths[_ENV_CERT], "client certificate"),
    )


def channel_credentials_from_store(org: str, caller: str) -> tuple[grpc.ChannelCredentials, str]:
    """Build channel credentials from the credential store.

    Returns the credentials and the endpoint recorded beside them, so a caller
    that enrolled with ``tide login`` needs no address of its own.
    """
    stored = credstore.load(org, caller)
    creds = grpc.ssl_channel_credentials(
        root_certificates=stored.ca,
        private_key=stored.private_key,
        certificate_chain=stored.certificate_chain,
    )
    return creds, stored.endpoint


def connect(target: str, *, options: tuple[tuple[str, int], ...] | None = None) -> grpc.Channel:
    """Open a channel to target using the env-var contract."""
    return grpc.secure_channel(
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
) -> grpc.Channel:
    """Open a channel using the credential store, defaulting to its endpoint."""
    creds, endpoint = channel_credentials_from_store(org, caller)
    address = target or endpoint
    if not address:
        raise TransportError(
            f"no endpoint recorded for {org}/{caller} and none given. "
            "Pass target=, or re-run `tide login` to record one."
        )
    return grpc.secure_channel(
        address,
        creds,
        options=list(options if options is not None else CHANNEL_OPTIONS),
    )


def _read(path: str, what: str) -> bytes:
    try:
        return Path(path).read_bytes()
    except OSError as exc:
        raise TransportError(f"read {what} from {path}: {exc}") from exc
