"""The worker's session behaviour, driven against a real gRPC server.

A fake server rather than mocks: the things worth testing here are ordering,
concurrency and what actually reaches the wire, and a mock asserts only that
the code called what the test expected it to call.
"""

from __future__ import annotations

import json
import queue
import threading
import time
from concurrent import futures
from typing import Any

import grpc
import pytest

from atlantis_client._wire import frame_pb2
from atlantis_client.jobs import DispatchedWorker, Registry, WorkerConfig, checkpoint
from atlantis_client.jobs import _envelopes as env
from atlantis_client.jobs.worker import (
    _CTRL,
    _DATA,
    _STOP,
    FRAMED_METHOD,
    _Inflight,
    _Outbound,
)

# --- a fake dispatcher ----------------------------------------------------


class FakeDispatcher:
    """Serves the framed method, scripted per test.

    Records every envelope the worker sends, so a test asserts on what
    actually crossed the wire.
    """

    def __init__(self, script: list[dict[str, Any]]) -> None:
        self.script = script
        self.received: list[dict[str, Any]] = []
        self.opened = threading.Event()
        self._done = threading.Event()

    def handler(self, request_iterator: Any, context: Any) -> Any:
        first = next(iter(request_iterator))
        self.received.append(json.loads(first.json))
        self.opened.set()

        for body in self.script:
            yield frame_pb2.Frame(json=json.dumps(body).encode())

        # Then drain whatever the worker sends until it stops or the test is
        # satisfied. Runs on the server thread, so it must not block for ever.
        deadline = time.monotonic() + 5
        for frame in request_iterator:
            self.received.append(json.loads(frame.json))
            if self._done.is_set() or time.monotonic() > deadline:
                break

    def envelopes(self, kind: str) -> list[dict[str, Any]]:
        return [e[kind] for e in self.received if kind in e]

    def wait_for(self, kind: str, timeout: float = 5.0) -> dict[str, Any]:
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            found = self.envelopes(kind)
            if found:
                return found[0]
            time.sleep(0.01)
        raise AssertionError(f"no {kind} envelope arrived; got {self.received}")

    def finish(self) -> None:
        self._done.set()


def serve(dispatcher: FakeDispatcher) -> tuple[grpc.Server, str]:
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    handler = grpc.method_handlers_generic_handler(
        "atlantis.workerdispatch.v1.WorkerDispatchFramed",
        {
            "WorkerSession": grpc.stream_stream_rpc_method_handler(
                dispatcher.handler,
                request_deserializer=frame_pb2.Frame.FromString,
                response_serializer=lambda f: f.SerializeToString(),
            )
        },
    )
    server.add_generic_rpc_handlers((handler,))
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    return server, f"127.0.0.1:{port}"


def accepted(heartbeat_ms: int = 60000) -> dict[str, Any]:
    return {
        "session_accepted": {
            "session_id": "s1",
            "lease_ttl_ms": 30000,
            "heartbeat_ms": heartbeat_ms,
        }
    }


def dispatch(job_id: int = 7, args: bytes = b"{}", timeout_ms: int = 0) -> dict[str, Any]:
    import base64

    return {
        "dispatch": {
            "job_id": job_id,
            "job_name": "library.ReindexAuthor",
            "queue": "maintenance",
            "args": base64.b64encode(args).decode(),
            "attempts": 1,
            "max_retries": 3,
            "timeout_ms": timeout_ms,
        }
    }


def run_worker(target: str, registry: Registry, **cfg: Any) -> DispatchedWorker:
    channel = grpc.insecure_channel(target)
    worker = DispatchedWorker(
        channel, registry, WorkerConfig(queue="maintenance", pod_id="test", **cfg)
    )
    threading.Thread(target=worker.run, daemon=True).start()
    return worker


# --- the tests ------------------------------------------------------------


def test_open_declares_the_registered_jobs() -> None:
    reg = Registry()
    reg.register("library.ReindexAuthor", lambda a: None)
    d = FakeDispatcher([accepted()])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        assert d.opened.wait(5), "the worker never sent Open"
        opened = d.received[0]["open"]
        assert opened["queue"] == "maintenance"
        assert opened["job_names"] == ["library.ReindexAuthor"]
        assert opened["pod_id"] == "test"
        worker.stop()
    finally:
        d.finish()
        server.stop(0)


def test_a_job_runs_and_reports_ack_then_complete() -> None:
    ran = threading.Event()
    seen_args: dict[str, Any] = {}

    def handler(args: Any) -> None:
        seen_args.update(args)
        ran.set()

    reg = Registry()
    reg.register("library.ReindexAuthor", handler)
    d = FakeDispatcher([accepted(), dispatch(args=b'{"author_id":3}')])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        assert ran.wait(5), "the handler never ran"
        assert seen_args == {"author_id": 3}, seen_args
        # Ack must precede Complete: the server revokes a row whose Ack does
        # not arrive within half the lease.
        d.wait_for("ack")
        d.wait_for("complete")
        kinds = [k for e in d.received for k in e]
        assert kinds.index("ack") < kinds.index("complete"), kinds
        worker.stop()
    finally:
        d.finish()
        server.stop(0)


def test_a_raising_handler_reports_fail_with_retry() -> None:
    def handler(args: Any) -> None:
        raise RuntimeError("boom")

    reg = Registry()
    reg.register("library.ReindexAuthor", handler)
    d = FakeDispatcher([accepted(), dispatch()])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        failed = d.wait_for("fail")
        assert failed["job_id"] == 7
        assert "boom" in failed["error"]
        assert failed["retry"] is True
        worker.stop()
    finally:
        d.finish()
        server.stop(0)


def test_an_unregistered_job_fails_rather_than_hanging() -> None:
    """The row must come back, not sit until its lease expires."""
    reg = Registry()
    reg.register("library.Other", lambda a: None)
    d = FakeDispatcher([accepted(), dispatch()])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        failed = d.wait_for("fail")
        assert "no handler registered" in failed["error"]
        worker.stop()
    finally:
        d.finish()
        server.stop(0)


def test_a_duplicate_dispatch_does_not_start_a_second_handler() -> None:
    """The server can re-dispatch an id already running here.

    Starting a second handler stacks two runs of one job and inflates the
    in-flight count — the concurrent-handler stacking the Go worker records as
    an incident fix.
    """
    starts = threading.Semaphore(0)
    release = threading.Event()
    runs = []

    def handler(args: Any) -> None:
        runs.append(1)
        starts.release()
        release.wait(5)

    reg = Registry()
    reg.register("library.ReindexAuthor", handler)
    # The same job id twice, back to back.
    d = FakeDispatcher([accepted(), dispatch(job_id=7), dispatch(job_id=7)])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        assert starts.acquire(timeout=5), "the first handler never started"
        # Give the second dispatch time to be mishandled if the guard is gone.
        time.sleep(0.5)
        assert len(runs) == 1, f"the duplicate started a second handler: {len(runs)} runs"
        # Both dispatches are acked — the second re-arms the server's clock.
        assert len(d.envelopes("ack")) == 2, d.envelopes("ack")
        release.set()
        worker.stop()
    finally:
        release.set()
        d.finish()
        server.stop(0)


def test_revoke_cancels_and_suppresses_the_terminal() -> None:
    """A revoked job's Complete must not be sent.

    The server has already released the row; a late Complete would mark a job
    done that another worker may now be running.
    """
    started = threading.Event()
    release = threading.Event()

    def handler(args: Any) -> None:
        started.set()
        release.wait(5)

    reg = Registry()
    reg.register("library.ReindexAuthor", handler)
    d = FakeDispatcher(
        [accepted(), dispatch(job_id=7), {"revoke": {"job_id": 7, "reason": "lease_expired"}}]
    )
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        assert started.wait(5)
        time.sleep(0.3)  # let the revoke land
        release.set()
        time.sleep(0.5)  # let the handler finish and try to report
        assert d.envelopes("complete") == [], "a revoked job still reported Complete"
        worker.stop()
    finally:
        release.set()
        d.finish()
        server.stop(0)


def test_checkpoint_from_a_handler_reaches_the_server() -> None:
    """checkpoint() travels by ContextVar set on the pool thread.

    Set on the submitting thread instead, this is invisible here and every
    call silently does nothing.
    """
    reg = Registry()
    reg.register("library.ReindexAuthor", lambda a: checkpoint(42, "halfway"))
    d = FakeDispatcher([accepted(), dispatch()])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        cp = d.wait_for("checkpoint")
        assert cp["job_id"] == 7
        assert cp["pct"] == 42
        assert cp["msg"] == "halfway"
        worker.stop()
    finally:
        d.finish()
        server.stop(0)


def test_heartbeat_names_the_inflight_jobs() -> None:
    release = threading.Event()
    started = threading.Event()

    def handler(args: Any) -> None:
        started.set()
        release.wait(5)

    reg = Registry()
    reg.register("library.ReindexAuthor", handler)
    # A short heartbeat so the test does not wait the 60s default.
    d = FakeDispatcher([accepted(heartbeat_ms=100), dispatch(job_id=11)])
    server, target = serve(d)
    try:
        worker = run_worker(target, reg)
        assert started.wait(5)
        beat = d.wait_for("heartbeat")
        assert beat["job_ids"] == [11], beat
        release.set()
        worker.stop()
    finally:
        release.set()
        d.finish()
        server.stop(0)


# --- the generation counter -----------------------------------------------
#
# Driven directly rather than through a stream: the case is a race — a handler
# unwinding while the same id is revoked and re-dispatched — and a timing test
# for it would pass whether or not the guard was there.


def _worker_for_untrack() -> DispatchedWorker:
    reg = Registry()
    reg.register("library.ReindexAuthor", lambda a: None)
    return DispatchedWorker(
        grpc.insecure_channel("127.0.0.1:1"), reg, WorkerConfig(queue="maintenance")
    )


def test_untrack_removes_the_entry_it_was_tracked_under() -> None:
    w = _worker_for_untrack()
    with w._inflight_lock:
        w._generation += 1
        gen = w._generation
        w._inflight[7] = _Inflight(gen, threading.Event())

    w._untrack(7, gen)
    assert 7 not in w._inflight


def test_a_late_handler_does_not_clear_a_newer_dispatch() -> None:
    """The id was revoked and re-dispatched while the first handler unwound.

    Deleting on the way out would clear the *new* dispatch's entry, and its
    Revoke would then find nothing to cancel while its handler kept running.
    """
    w = _worker_for_untrack()
    with w._inflight_lock:
        w._generation += 1
        first = w._generation
        # The re-dispatch: same id, a newer generation.
        w._generation += 1
        second = w._generation
        w._inflight[7] = _Inflight(second, threading.Event())

    # The first handler finishes now and tries to untrack.
    w._untrack(7, first)

    assert 7 in w._inflight, "the late handler cleared the newer dispatch's entry"
    assert w._inflight[7].generation == second


# --- the priority queue, unit level ---------------------------------------


def test_control_envelopes_sort_ahead_of_data() -> None:
    """Heartbeat and Checkpoint keep leases alive; Ack and Complete do not.

    A heartbeat stuck behind a burst of terminals means leases expire, the
    server revokes, and it re-dispatches work still running here.
    """
    import queue as _queue

    q: _queue.PriorityQueue[_Outbound] = _queue.PriorityQueue()
    q.put(_Outbound(_DATA, 1, {"complete": {"job_id": 1}}))
    q.put(_Outbound(_DATA, 2, {"complete": {"job_id": 2}}))
    q.put(_Outbound(_CTRL, 3, {"heartbeat": {"job_ids": [1, 2]}}))

    assert next(iter(q.get().body)) == "heartbeat", "the heartbeat did not jump the queue"


def test_ordering_never_compares_the_payload() -> None:
    """Two dicts are not orderable.

    Without the sequence number a tie on priority would raise TypeError inside
    heapq, which surfaces from a queue put rather than from anything the
    reader can connect to the envelope.
    """
    import queue as _queue

    q: _queue.PriorityQueue[_Outbound] = _queue.PriorityQueue()
    q.put(_Outbound(_CTRL, 1, {"heartbeat": {"job_ids": [1]}}))
    q.put(_Outbound(_CTRL, 2, {"heartbeat": {"job_ids": [2]}}))
    # Same priority, so the tiebreak is the sequence, and arrival order holds.
    assert q.get().seq == 1
    assert q.get().seq == 2


# --- configuration --------------------------------------------------------


def test_max_in_flight_is_clamped_to_the_server_ceiling() -> None:
    cfg = WorkerConfig(queue="q", max_in_flight=10_000)
    assert cfg.max_in_flight == env.MAX_IN_FLIGHT_CEILING


@pytest.mark.parametrize(
    ("kwargs", "match"),
    [({"queue": ""}, "queue"), ({"queue": "q", "max_in_flight": 0}, "max_in_flight")],
)
def test_invalid_configuration_is_refused(kwargs: dict[str, Any], match: str) -> None:
    with pytest.raises(ValueError, match=match):
        WorkerConfig(**kwargs)


def test_an_empty_registry_is_refused() -> None:
    """Open would declare no job names and the server would close the session.

    Refused here, where the message can say why, rather than as a stream
    error a few hundred milliseconds later.
    """
    with pytest.raises(ValueError, match="empty"):
        DispatchedWorker(grpc.insecure_channel("127.0.0.1:1"), Registry(), WorkerConfig(queue="q"))


def test_the_framed_method_matches_the_server() -> None:
    """jobsdispatcher.FramedMethodPath() builds the same string."""
    assert FRAMED_METHOD == "/atlantis.workerdispatch.v1.WorkerDispatchFramed/WorkerSession"


# --- the outbound queue ---------------------------------------------------


def idle_worker(**cfg: Any) -> DispatchedWorker:
    """A worker that has never opened a stream, so nothing drains its queue."""
    reg = Registry()
    reg.register("library.ReindexAuthor", lambda a: None)
    channel = grpc.insecure_channel("127.0.0.1:1")
    return DispatchedWorker(channel, reg, WorkerConfig(queue="maintenance", **cfg))


def test_stopping_a_worker_with_envelopes_queued_does_not_raise() -> None:
    """stop() put None, and PriorityQueue orders with heapq.

    Comparing None against an _Outbound raises TypeError, so stopping a
    worker that still had anything queued — any worker under load — failed
    instead of stopping. An idle queue never compares two items, which is why
    every session test above passed.
    """
    worker = idle_worker()
    worker._send_data(env.ack(1))
    worker._send_data(env.complete(1))

    worker.stop()

    drained = [worker._outbound.get_nowait() for _ in range(3)]
    assert [d.priority for d in drained] == [_DATA, _DATA, _STOP]


def test_the_stop_envelope_sorts_after_the_work_already_queued() -> None:
    """A completion dropped at shutdown is a job the server redelivers."""
    worker = idle_worker()
    worker.stop()
    worker._send_data(env.complete(4))
    worker._send_ctrl(env.heartbeat([4]))

    drained = [worker._outbound.get_nowait() for _ in range(3)]
    assert [d.priority for d in drained] == [_CTRL, _DATA, _STOP]


def test_the_sender_drains_its_own_session_queue_not_the_current_one() -> None:
    """gRPC consumes the request iterator on its own thread, and that thread
    stays parked in get() after the call ends.

    Reading self._outbound would hand a finished session's generator the
    envelopes of the session that replaced it, and write them to a stream
    that is already closed. An ack the server never sees is a job it
    redelivers to another worker while this one runs it.
    """
    worker = idle_worker()
    dead = worker._outbound
    frames = worker._outbound_frames(dead)

    # What _run_once does on reconnect, then a live session's ack and a stop.
    worker._outbound = queue.PriorityQueue()
    worker._send_data(env.ack(99))
    worker.stop()

    # The dead queue is empty and _stopping is set, so this generator ends
    # having sent nothing. Reading self._outbound instead yields the ack.
    assert list(frames) == []
    assert worker._outbound.qsize() == 2, "the dead session drained the live queue"


def test_a_full_queue_still_stops() -> None:
    """stop()'s envelope is dropped when the queue is full, so it cannot be
    the only exit — the sender polls for _stopping as well."""
    worker = idle_worker(max_in_flight=1)
    full = worker._outbound
    while not full.full():
        worker._send_data(env.ack(full.qsize()))
    queued = full.qsize()

    worker.stop()

    # On its own thread: without the poll the sender parks in get() for ever,
    # and this must report that rather than hang the suite.
    sent: list[Any] = []
    drain = threading.Thread(target=lambda: sent.extend(worker._outbound_frames(full)), daemon=True)
    drain.start()
    drain.join(5)

    assert not drain.is_alive(), "the sender never noticed the stop"
    assert len(sent) == queued
