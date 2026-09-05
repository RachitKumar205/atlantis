"""Reading the credential store."""

from __future__ import annotations

from pathlib import Path

import pytest

from atlantis_client import credentials as cred

KEY = b"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----"
LEAF = b"-----BEGIN CERTIFICATE-----\nCCCC\n-----END CERTIFICATE-----"


def _write_store(root: Path, org: str = "acme", caller: str = "api") -> Path:
    directory = root / org / caller
    directory.mkdir(parents=True)
    (directory / "client.pem").write_bytes(KEY + b"\n" + LEAF + b"\n")
    (directory / "ca.crt").write_bytes(
        b"-----BEGIN CERTIFICATE-----\nEEEE\n-----END CERTIFICATE-----\n"
    )
    (directory / "endpoint").write_text("atlantis.example.com:8443\n")
    (directory / "enroll_url").write_text("https://enroll.example.com\n")
    return directory


def test_atlantis_home_overrides_the_default_root(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    assert cred.store_root() == tmp_path


def test_default_root_is_dot_atlantis_under_home(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("ATLANTIS_HOME", raising=False)
    assert cred.store_root().name == ".atlantis"


def test_load_reads_and_splits(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    _write_store(tmp_path)

    got = cred.load("acme", "api")
    assert got.org == "acme"
    assert got.caller == "api"
    assert b"PRIVATE KEY" in got.private_key
    assert b"CERTIFICATE" in got.certificate_chain
    assert b"PRIVATE KEY" not in got.certificate_chain
    assert got.endpoint == "atlantis.example.com:8443"
    assert got.enroll_url == "https://enroll.example.com"


def test_endpoint_is_trimmed(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """The file ends in a newline; a target carrying one fails to resolve."""
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    directory = _write_store(tmp_path)
    (directory / "endpoint").write_text("  host:1234  \n\n")
    assert cred.load("acme", "api").endpoint == "host:1234"


def test_absent_entry_names_the_login_command(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    with pytest.raises(cred.NoCredentials, match="tide login"):
        cred.load("acme", "api")


def test_missing_ca_is_not_reported_as_missing_credentials(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """A half-written store is its own failure.

    Reporting it as NoCredentials would send the reader to `tide login` for an
    entry that exists, and the message would be wrong about which file is at
    fault.
    """
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    directory = _write_store(tmp_path)
    (directory / "ca.crt").unlink()
    with pytest.raises(cred.CredentialsError) as excinfo:
        cred.load("acme", "api")
    assert not isinstance(excinfo.value, cred.NoCredentials)
    assert "ca.crt" in str(excinfo.value)


@pytest.mark.parametrize(
    "name",
    ["", "-lead", "trail-", "Upper", "has_underscore", "has.dot", "a" * 65, "..", "a/b"],
)
def test_invalid_store_names_are_refused(
    name: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """The organisation and the caller both become path components.

    `..` in either would read outside the store, and both arrive from outside:
    the organisation from an argument, the caller from a server response.
    """
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    with pytest.raises(cred.CredentialsError):
        cred.credential_dir(name, "api")
    with pytest.raises(cred.CredentialsError):
        cred.credential_dir("acme", name)


@pytest.mark.parametrize("name", ["a", "acme", "acme-corp", "a-b-c", "x9", "9x", "a" * 64])
def test_valid_store_names_are_accepted(
    name: str, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    assert cred.credential_dir(name, name).parts[-2:] == (name, name)
