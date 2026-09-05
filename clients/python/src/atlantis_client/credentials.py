"""Reading what ``tide login`` wrote.

The store, which this only reads:

    ~/.atlantis/<org>/<caller>/
        client.pem    private key + leaf, 0600
        ca.crt        the root that verifies this organisation's atlantis
        endpoint      the address callers dial
        enroll_url    where to renew

``ATLANTIS_HOME`` replaces ``~/.atlantis``.

Renewal belongs to ``tide``. Nothing here writes to the store or contacts the
enrolment listener; a client that finds an expired certificate reports it and
says which command replaces it.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

from ._pem import split_client_pem

__all__ = ["CredentialsError", "NoCredentials", "StoredCredentials", "load", "store_root"]


class CredentialsError(Exception):
    """The store could not be read, or holds something unusable."""


class NoCredentials(CredentialsError):
    """Nothing has been enrolled for this organisation and caller."""


_CLIENT_PEM = "client.pem"
_CA_PEM = "ca.crt"
_ENDPOINT = "endpoint"
_ENROLL_URL = "enroll_url"


@dataclass(frozen=True)
class StoredCredentials:
    """One caller's material, as read off disk."""

    org: str
    caller: str
    directory: Path
    private_key: bytes
    certificate_chain: bytes
    ca: bytes
    endpoint: str
    enroll_url: str


def store_root() -> Path:
    """The directory the store lives under."""
    override = os.environ.get("ATLANTIS_HOME")
    if override:
        return Path(override)
    return Path.home() / ".atlantis"


def _valid_store_name(name: str) -> bool:
    """The grammar for a path component in the store.

    Lowercase letters, digits, and hyphens that are neither first nor last;
    1 to 64 characters. The same rule atlantis enforces for caller names.

    Both the organisation and the caller are joined into a filesystem path and
    both arrive from outside — the organisation from an argument, the caller
    from a server response — so `..` has to be impossible rather than unlikely.
    """
    if not name or len(name) > 64:
        return False
    for i, ch in enumerate(name):
        if "a" <= ch <= "z" or "0" <= ch <= "9":
            continue
        if ch == "-" and 0 < i < len(name) - 1:
            continue
        return False
    return True


def credential_dir(org: str, caller: str) -> Path:
    """Where one caller's credentials live."""
    if not _valid_store_name(org):
        raise CredentialsError(
            f"{org!r} is not a valid organisation name "
            "(lowercase letters, digits and interior hyphens)"
        )
    if not _valid_store_name(caller):
        raise CredentialsError(
            f"{caller!r} is not a valid caller name "
            "(lowercase letters, digits and interior hyphens)"
        )
    return store_root() / org / caller


def load(org: str, caller: str) -> StoredCredentials:
    """Read one caller's material, splitting client.pem into its two parts."""
    directory = credential_dir(org, caller)
    try:
        client_pem = (directory / _CLIENT_PEM).read_bytes()
    except FileNotFoundError as exc:
        raise NoCredentials(
            f"no credentials for {org}/{caller} in {directory}. "
            f"Run `tide login --org {org}` to enrol this machine."
        ) from exc

    try:
        ca = (directory / _CA_PEM).read_bytes()
    except FileNotFoundError as exc:
        raise CredentialsError(
            f"{directory / _CA_PEM} is missing, so the server's certificate "
            "cannot be verified. Re-run `tide login` to rewrite the store."
        ) from exc

    key, chain = split_client_pem(client_pem)
    return StoredCredentials(
        org=org,
        caller=caller,
        directory=directory,
        private_key=key,
        certificate_chain=chain,
        ca=ca,
        endpoint=_read_trimmed(directory / _ENDPOINT),
        enroll_url=_read_trimmed(directory / _ENROLL_URL),
    )


def _read_trimmed(path: Path) -> str:
    try:
        return path.read_text(encoding="utf-8").strip()
    except OSError:
        return ""
