"""Runtime for the typed Python client ``tide generate`` writes.

The generated code lives in the caller's repository under its own ``atlantis``
package and imports this one. This package is published as ``atlantis-client``
and imported as ``atlantis_client``; the two names differ because the generated
tree owns the top-level ``atlantis`` name.

Typical use::

    from atlantis_client import connect
    from atlantis.library.v1 import AuthorClient, GetAuthorRequest

    with connect("atlantis.example.com:8443") as channel:
        authors = AuthorClient(channel)
        author = authors.get(GetAuthorRequest(id=7)).entity

The async flavour is :mod:`atlantis_client.aio`, whose ``connect`` returns a
``grpc.aio`` channel and whose generated clients are the ``Async`` classes.
"""

from __future__ import annotations

from .credentials import CredentialsError, NoCredentials, StoredCredentials, load, store_root
from .errors import (
    AlreadyExists,
    AtlantisError,
    Conflict,
    DeadlineExceeded,
    Internal,
    InvalidArgument,
    NotFound,
    PermissionDenied,
    ResourceExhausted,
    Unauthenticated,
    Unavailable,
    Unimplemented,
    UnsupportedQueryOption,
    translate,
)
from .pagination import SERVER_MAX_LIMIT, aiterate_pages, iterate_pages
from .tenant import PARTITION_HEADER, metadata_for
from .transport import (
    CHANNEL_OPTIONS,
    MAX_MESSAGE_BYTES,
    TransportError,
    channel_credentials,
    channel_credentials_from_store,
    connect,
    connect_from_store,
)

__version__ = "0.1.0"

__all__ = [
    "CHANNEL_OPTIONS",
    "MAX_MESSAGE_BYTES",
    "PARTITION_HEADER",
    "SERVER_MAX_LIMIT",
    "AlreadyExists",
    "AtlantisError",
    "Conflict",
    "CredentialsError",
    "DeadlineExceeded",
    "Internal",
    "InvalidArgument",
    "NoCredentials",
    "NotFound",
    "PermissionDenied",
    "ResourceExhausted",
    "StoredCredentials",
    "TransportError",
    "Unauthenticated",
    "Unavailable",
    "Unimplemented",
    "UnsupportedQueryOption",
    "__version__",
    "aiterate_pages",
    "channel_credentials",
    "channel_credentials_from_store",
    "connect",
    "connect_from_store",
    "iterate_pages",
    "load",
    "metadata_for",
    "store_root",
    "translate",
]
