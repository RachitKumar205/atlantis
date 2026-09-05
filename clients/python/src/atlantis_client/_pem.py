"""Splitting the credential store's combined client.pem.

``tide login`` writes the private key and the leaf certificate into one file,
so renewal replaces both under a single atomic rename. Go hands that one buffer
to ``tls.X509KeyPair`` twice and it finds both blocks itself.

``grpc.ssl_channel_credentials`` takes the key and the chain as separate
arguments, so the blocks are separated here.
"""

from __future__ import annotations

import re

__all__ = ["PEMError", "split_client_pem"]


class PEMError(ValueError):
    """A PEM file held none of what was asked of it, or could not be parsed."""


# One PEM block. DOTALL so the base64 body's newlines are inside the match, and
# the label is captured so BEGIN and END can be required to agree — a truncated
# file otherwise matches across two blocks and yields a body that is neither.
_BLOCK = re.compile(
    rb"-----BEGIN (?P<label>[A-Z0-9 ]+)-----.*?-----END (?P=label)-----",
    re.DOTALL,
)


def split_client_pem(pem: bytes) -> tuple[bytes, bytes]:
    """Return ``(private_key, certificate_chain)`` from a combined PEM.

    The chain keeps every CERTIFICATE block in file order: the leaf comes
    first and any intermediates follow it, which is the order TLS requires and
    the order the file already carries.

    Raises PEMError when either part is absent.
    """
    key_blocks: list[bytes] = []
    cert_blocks: list[bytes] = []

    for match in _BLOCK.finditer(pem):
        label = match.group("label").decode("ascii")
        block = match.group(0)
        if label == "CERTIFICATE":
            cert_blocks.append(block)
        elif "PRIVATE KEY" in label:
            # Matches Go's rule: PKCS#1 ("RSA PRIVATE KEY"), SEC 1 ("EC PRIVATE
            # KEY") and PKCS#8 ("PRIVATE KEY") all qualify.
            key_blocks.append(block)

    if not key_blocks:
        raise PEMError(
            "client.pem holds no private key block. It should carry the key and "
            "the leaf certificate together; re-run `tide login` to rewrite it."
        )
    if not cert_blocks:
        raise PEMError(
            "client.pem holds no certificate block. It should carry the key and "
            "the leaf certificate together; re-run `tide login` to rewrite it."
        )
    return _join(key_blocks[:1]), _join(cert_blocks)


def _join(blocks: list[bytes]) -> bytes:
    return b"\n".join(blocks) + b"\n"
