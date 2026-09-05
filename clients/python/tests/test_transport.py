"""The env-var contract and the channel options."""

from __future__ import annotations

from pathlib import Path

import pytest

from atlantis_client import transport

KEY = b"-----BEGIN PRIVATE KEY-----\nAAAA\n-----END PRIVATE KEY-----\n"
LEAF = b"-----BEGIN CERTIFICATE-----\nCCCC\n-----END CERTIFICATE-----\n"

ENV = ("ATL_TLS_CERT", "ATL_TLS_KEY", "ATL_TLS_CA")


@pytest.fixture()
def material(tmp_path: Path) -> dict[str, str]:
    (tmp_path / "client.crt").write_bytes(LEAF)
    (tmp_path / "client.key").write_bytes(KEY)
    (tmp_path / "ca.crt").write_bytes(LEAF)
    return {
        "ATL_TLS_CERT": str(tmp_path / "client.crt"),
        "ATL_TLS_KEY": str(tmp_path / "client.key"),
        "ATL_TLS_CA": str(tmp_path / "ca.crt"),
    }


@pytest.mark.parametrize("absent", ENV)
def test_each_missing_variable_is_named(
    absent: str, material: dict[str, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    """The error names the variable that is unset, and all three that matter.

    A caller with one of three set is the common case, and an error that says
    only "TLS is not configured" sends them to check what is already right.
    """
    for name, value in material.items():
        monkeypatch.setenv(name, value)
    monkeypatch.delenv(absent)

    with pytest.raises(transport.TransportError) as excinfo:
        transport.channel_credentials()
    assert absent in str(excinfo.value)


def test_no_insecure_fallback(monkeypatch: pytest.MonkeyPatch) -> None:
    """Nothing unset yields a channel.

    Falling back to an insecure channel would replace a legible configuration
    error with a handshake failure one layer below where the cause is readable
    — and atlantis reads the caller's identity from the client certificate, so
    a plaintext channel has no identity to be authorized.
    """
    for name in ENV:
        monkeypatch.delenv(name, raising=False)
    with pytest.raises(transport.TransportError):
        transport.channel_credentials()
    with pytest.raises(transport.TransportError):
        transport.connect("localhost:1")


def test_unreadable_file_names_the_path(
    material: dict[str, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    for name, value in material.items():
        monkeypatch.setenv(name, value)
    monkeypatch.setenv("ATL_TLS_CA", "/nonexistent/ca.crt")
    with pytest.raises(transport.TransportError, match="/nonexistent/ca.crt"):
        transport.channel_credentials()


def test_credentials_build_from_the_env(
    material: dict[str, str], monkeypatch: pytest.MonkeyPatch
) -> None:
    for name, value in material.items():
        monkeypatch.setenv(name, value)
    assert transport.channel_credentials() is not None


def test_message_limits_are_raised_from_the_grpc_default() -> None:
    """64 MiB, both directions.

    grpc's own default is 4 MiB. One page of a Query carrying large jsonb or
    bytea columns overflows it, and no page size the caller sets fixes that —
    the limit is on the wire message, not the row count.
    """
    options = dict(transport.CHANNEL_OPTIONS)
    assert options["grpc.max_receive_message_length"] == 64 << 20
    assert options["grpc.max_send_message_length"] == 64 << 20
    assert transport.MAX_MESSAGE_BYTES > 4 << 20


def test_store_channel_without_an_endpoint_is_refused(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    directory = tmp_path / "acme" / "api"
    directory.mkdir(parents=True)
    (directory / "client.pem").write_bytes(KEY + LEAF)
    (directory / "ca.crt").write_bytes(LEAF)

    with pytest.raises(transport.TransportError, match="no endpoint"):
        transport.connect_from_store("acme", "api")


def test_store_channel_uses_the_recorded_endpoint(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setenv("ATLANTIS_HOME", str(tmp_path))
    directory = tmp_path / "acme" / "api"
    directory.mkdir(parents=True)
    (directory / "client.pem").write_bytes(KEY + LEAF)
    (directory / "ca.crt").write_bytes(LEAF)
    (directory / "endpoint").write_text("atlantis.example.com:8443\n")

    channel = transport.connect_from_store("acme", "api")
    channel.close()
