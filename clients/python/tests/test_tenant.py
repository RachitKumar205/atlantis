"""The tenant header."""

from __future__ import annotations

import pytest

from atlantis_client import tenant


def test_header_name_matches_the_server() -> None:
    """interceptors.PartitionHeader. A mismatch reads as no header at all.

    The server treats an absent header as absent rather than as an error, so a
    misspelling here does not fail loudly: unpartitioned entities keep working
    and partitioned ones come back Unauthenticated.
    """
    assert tenant.PARTITION_HEADER == "atlantis-tenant"


def test_metadata_carries_the_tenant() -> None:
    assert tenant.metadata_for("acme") == (("atlantis-tenant", "acme"),)


def test_the_value_is_trimmed() -> None:
    """The server trims before reading, so an untrimmed value would differ.

    Two spellings of one tenant would then key two rows of cache and two
    partitions of the same name.
    """
    assert tenant.metadata_for("  acme \n") == (("atlantis-tenant", "acme"),)


@pytest.mark.parametrize("blank", ["", "   ", "\t", "\n"])
def test_a_blank_tenant_is_refused(blank: str) -> None:
    """The server reads whitespace-only as absent.

    Sending it produces Unauthenticated from the server, which reads as a
    credentials problem rather than as the empty argument it is.
    """
    with pytest.raises(ValueError, match="tenant is empty"):
        tenant.metadata_for(blank)
