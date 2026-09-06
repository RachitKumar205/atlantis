"""The wire envelopes, especially the base64 boundary."""

from __future__ import annotations

import base64
import json

import pytest

from atlantis_client.jobs import _envelopes as env


def _dispatch(**overrides: object) -> bytes:
    """One server → worker Dispatch, encoded the way Go's json.Marshal does."""
    body = {
        "job_id": 7,
        "job_name": "library.ReindexAuthor",
        "queue": "maintenance",
        # Go []byte -> base64 string. This is the trap.
        "args": base64.b64encode(b'{"author_id":3}').decode(),
        "attempts": 1,
        "max_retries": 3,
        "timeout_ms": 30000,
    }
    body.update(overrides)  # type: ignore[arg-type]
    return json.dumps({"dispatch": body}).encode()


def test_args_are_base64_then_json() -> None:
    """The single most likely thing to get silently wrong in this port.

    Reading `args` as the payload hands the handler the string
    'eyJhdXRob3JfaWQiOjN9', which parses as neither JSON nor anything else —
    and a handler that only reads args.get("author_id") sees None rather than
    an error.
    """
    got = env.decode_server_envelope(_dispatch())
    assert isinstance(got, env.Dispatch)
    assert got.args == {"author_id": 3}, got.args
    assert got.job_id == 7
    assert got.timeout_ms == 30000


def test_trace_ctx_is_base64_bytes() -> None:
    raw = _dispatch(trace_ctx=base64.b64encode(b"trace-parent").decode())
    got = env.decode_server_envelope(raw)
    assert isinstance(got, env.Dispatch)
    assert got.trace_ctx == b"trace-parent"


@pytest.mark.parametrize("absent", [{}, {"args": ""}])
def test_a_job_with_no_args_decodes_to_an_empty_mapping(absent: dict[str, object]) -> None:
    """Go omits the key for a nil slice and writes "" for an empty one.

    Both mean the same thing here. Raising on either would make every job
    declaring no args undeliverable.
    """
    body = json.loads(_dispatch())
    body["dispatch"].pop("args", None)
    body["dispatch"].update(absent)
    got = env.decode_server_envelope(json.dumps(body).encode())
    assert isinstance(got, env.Dispatch)
    assert got.args == {}


def test_args_that_are_not_base64_are_reported() -> None:
    raw = _dispatch(args="not base64 at all!!")
    with pytest.raises(env.ProtocolError, match="base64"):
        env.decode_server_envelope(raw)


def test_args_that_decode_to_a_non_object_are_reported() -> None:
    raw = _dispatch(args=base64.b64encode(b"[1,2,3]").decode())
    with pytest.raises(env.ProtocolError, match="not an object"):
        env.decode_server_envelope(raw)


def test_session_accepted_carries_the_lease_parameters() -> None:
    raw = json.dumps(
        {"session_accepted": {"session_id": "s1", "lease_ttl_ms": 30000, "heartbeat_ms": 10000}}
    ).encode()
    got = env.decode_server_envelope(raw)
    assert isinstance(got, env.SessionAccepted)
    assert (got.session_id, got.lease_ttl_ms, got.heartbeat_ms) == ("s1", 30000, 10000)


def test_revoke_and_goodbye_decode() -> None:
    revoke = env.decode_server_envelope(
        json.dumps({"revoke": {"job_id": 9, "reason": "lease_expired"}}).encode()
    )
    assert isinstance(revoke, env.Revoke)
    assert (revoke.job_id, revoke.reason) == (9, "lease_expired")

    bye = env.decode_server_envelope(json.dumps({"goodbye": {"reason": "drain"}}).encode())
    assert isinstance(bye, env.Goodbye)
    assert bye.reason == "drain"


def test_an_unknown_envelope_is_reported_not_ignored() -> None:
    """Returning None would be indistinguishable from an idle stream.

    The session would then sit holding leases it is no longer being told
    about, which is the quiet shape this whole protocol is built to avoid.
    """
    with pytest.raises(env.ProtocolError, match="no known variant"):
        env.decode_server_envelope(json.dumps({"something_new": {}}).encode())


def test_malformed_json_is_reported() -> None:
    with pytest.raises(env.ProtocolError, match="not JSON"):
        env.decode_server_envelope(b"{not json")


def test_open_trims_job_names_to_the_server_cap() -> None:
    """The server closes a session whose Open exceeds the cap.

    Trimming here loses jobs, which is bad — but a refused session runs none
    of them, which is worse, and the cap is 1024.
    """
    names = [f"ns.Job{i}" for i in range(env.MAX_JOB_NAMES_PER_OPEN + 50)]
    body = env.open_session("q", names, 4, "pod", "v1")
    assert len(body["open"]["job_names"]) == env.MAX_JOB_NAMES_PER_OPEN


def test_heartbeat_trims_to_the_server_cap() -> None:
    ids = list(range(env.MAX_HEARTBEAT_IDS_PER_FRAME + 10))
    body = env.heartbeat(ids)
    assert len(body["heartbeat"]["job_ids"]) == env.MAX_HEARTBEAT_IDS_PER_FRAME


@pytest.mark.parametrize(("given", "want"), [(-5, 0), (0, 0), (50, 50), (100, 100), (250, 100)])
def test_checkpoint_pct_is_clamped(given: int, want: int) -> None:
    """The server clamps too; doing it here keeps the sent value and the
    stored value the same, so a console reading 100 is what was reported."""
    body = env.checkpoint_envelope(1, given, "")
    assert body["checkpoint"]["pct"] == want


def test_checkpoint_msg_is_truncated() -> None:
    body = env.checkpoint_envelope(1, 10, "x" * 500)
    assert len(body["checkpoint"]["msg"]) == env.MAX_CHECKPOINT_MSG_CHARS


def test_encode_matches_gos_compact_form() -> None:
    """No spaces after separators, as Go's json.Marshal writes.

    Nothing compares these bytes, but two encoders differing only in
    whitespace make a captured frame hard to diff against a Go one.
    """
    assert env.encode({"ack": {"job_id": 1}}) == b'{"ack":{"job_id":1}}'


def test_fail_carries_the_retry_decision() -> None:
    """retry=False sends the row to the dead-letter queue whatever attempts
    remain — the escape hatch for args a handler knows are unrecoverable."""
    assert env.fail(3, "bad args", retry=False)["fail"] == {
        "job_id": 3,
        "error": "bad args",
        "retry": False,
    }
