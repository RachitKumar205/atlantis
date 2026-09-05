"""Splitting the combined client.pem."""

from __future__ import annotations

import pytest

from atlantis_client._pem import PEMError, split_client_pem

KEY = b"-----BEGIN PRIVATE KEY-----\nAAAA\nBBBB\n-----END PRIVATE KEY-----"
LEAF = b"-----BEGIN CERTIFICATE-----\nCCCC\n-----END CERTIFICATE-----"
INTERMEDIATE = b"-----BEGIN CERTIFICATE-----\nDDDD\n-----END CERTIFICATE-----"


def test_splits_key_from_certificate() -> None:
    key, chain = split_client_pem(KEY + b"\n" + LEAF + b"\n")
    assert b"PRIVATE KEY" in key
    assert b"CERTIFICATE" in chain
    # The halves must not carry each other: grpc hands `private_key` and
    # `certificate_chain` to OpenSSL separately, and a chain containing the key
    # is what a naive "pass the whole file twice" port produces.
    assert b"CERTIFICATE" not in key
    assert b"PRIVATE KEY" not in chain


def test_order_is_independent() -> None:
    """tide writes the key first; a file written the other way still works."""
    key, chain = split_client_pem(LEAF + b"\n" + KEY + b"\n")
    assert b"PRIVATE KEY" in key
    assert b"CERTIFICATE" in chain


def test_chain_keeps_every_certificate_in_file_order() -> None:
    """An intermediate must survive, after the leaf.

    Keeping only the first certificate makes the chain unverifiable wherever
    the server does not already hold the intermediate, and that failure appears
    at the TLS handshake with no mention of this function.
    """
    _, chain = split_client_pem(KEY + b"\n" + LEAF + b"\n" + INTERMEDIATE + b"\n")
    assert chain.count(b"BEGIN CERTIFICATE") == 2
    assert chain.index(b"CCCC") < chain.index(b"DDDD")


@pytest.mark.parametrize("label", [b"RSA PRIVATE KEY", b"EC PRIVATE KEY", b"PRIVATE KEY"])
def test_every_private_key_label_go_accepts(label: bytes) -> None:
    """Go's rule is a label containing "PRIVATE KEY"; all three must qualify."""
    pem = b"-----BEGIN " + label + b"-----\nAAAA\n-----END " + label + b"-----\n" + LEAF
    key, _ = split_client_pem(pem)
    assert label in key


def test_missing_certificate_is_named() -> None:
    with pytest.raises(PEMError, match="no certificate block"):
        split_client_pem(KEY)


def test_missing_key_is_named() -> None:
    with pytest.raises(PEMError, match="no private key block"):
        split_client_pem(LEAF)


def test_a_truncated_key_block_is_refused_not_repaired() -> None:
    """BEGIN and END must name the same label.

    Without that, `BEGIN PRIVATE KEY` with no matching END matches across to the
    certificate's `-----END CERTIFICATE-----`, and the "key" handed to OpenSSL
    is the truncated key plus the whole leaf. That fails at the handshake, which
    says nothing about the file that caused it.

    Refusing is the outcome wanted: the message names client.pem.
    """
    truncated = b"-----BEGIN PRIVATE KEY-----\nAAAA\n" + LEAF
    with pytest.raises(PEMError, match="no private key block"):
        split_client_pem(truncated)
