"""The tenant header a partitioned entity requires.

An entity declaring ``partition by`` is served only to a request carrying
``atlantis-tenant``; without it the call comes back ``Unauthenticated``.

The server treats a whitespace-only value as absent and refuses two differing
values rather than choosing between them, so both are refused here — where the
message can name the argument rather than the header.
"""

from __future__ import annotations

__all__ = ["PARTITION_HEADER", "metadata_for"]

PARTITION_HEADER = "atlantis-tenant"


def metadata_for(tenant: str) -> tuple[tuple[str, str], ...]:
    """Return the call metadata asserting ``tenant``.

    Raises ValueError for an empty or whitespace-only tenant: the server reads
    that as no header at all, so the call would fail as though the argument had
    never been passed.
    """
    trimmed = tenant.strip()
    if not trimmed:
        raise ValueError(
            "tenant is empty. A partitioned entity is served only to a request "
            f"carrying a {PARTITION_HEADER} value, and a blank one reads as absent."
        )
    return ((PARTITION_HEADER, trimmed),)
