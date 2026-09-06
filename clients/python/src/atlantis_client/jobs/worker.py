"""The dispatched worker: one bidi stream, jobs pushed down it.

A port of clients/go/jobs/dispatched_worker.go. atlantis claims rows and
pushes them; this worker acks, runs the handler on a pool thread, and reports
the outcome. It never touches Postgres — the direct-PG worker the Go SDK also
ships is not ported, and will not be: it needs a driver and a second set of
credentials for a path the dispatcher already covers.

The stream is the framed service, because grpc-python cannot select the
content-subtype the unframed one negotiates with. See
atlantis/workerdispatch/v1/frame.proto.

Two behaviours here are recorded in the Go file as incident fixes and are
ported deliberately rather than rediscovered:

  - **Control envelopes outrank data.** Heartbeat and Checkpoint keep leases
    alive; Ack and Complete do not. When a burst of terminals fills the
    outbound queue, a heartbeat stuck behind them means leases expire, the
    server revokes, and it re-dispatches work this worker is still running.
  - **A generation counter guards untracking.** A handler finishing late must
    not delete an in-flight entry belonging to a newer dispatch of the same
    job id.
"""

from __future__ import annotations

import logging
import queue
import random
import threading
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

import grpc

from .._wire import frame_pb2
from . import _envelopes as env
from .checkpoint import use_checkpointer

if TYPE_CHECKING:
    from collections.abc import Iterator

    from .registry import Registry

__all__ = ["FRAMED_METHOD", "DispatchedWorker", "WorkerConfig"]

log = logging.getLogger("atlantis.jobs")

# The method the framed service registers. Hardcoded because the envelopes are
# JSON and there is no generated service stub to bind to; jobsdispatcher's
# FramedMethodPath() builds the same string from its ServiceDesc.
FRAMED_METHOD = "/atlantis.workerdispatch.v1.WorkerDispatchFramed/WorkerSession"

_BACKOFF_BASE_S = 1.0
_BACKOFF_CAP_S = 30.0
_BACKOFF_JITTER = 0.25

# How often the sender looks up from an empty queue to check for a stop.
_DRAIN_POLL_S = 0.2

# Priority classes for the outbound queue. Lower sorts first.
#
# _STOP sorts last so stop() flushes the acks and completes already queued
# before the stream closes; a row whose completion is dropped is redelivered.
_CTRL = 0
_DATA = 1
_STOP = 2


@dataclass
class WorkerConfig:
    """How this worker announces itself and how much it will run at once."""

    queue: str
    max_in_flight: int = 8
    pod_id: str = ""
    version: str = ""
    # Seconds to wait for in-flight handlers when stopping. Past it the worker
    # returns and leaves the rows to the server's lease expiry, which is what
    # a killed pod does anyway.
    shutdown_grace_s: float = 30.0

    def __post_init__(self) -> None:
        if not self.queue:
            raise ValueError("queue is required: a worker drains exactly one")
        if self.max_in_flight < 1:
            raise ValueError("max_in_flight must be at least 1")
        # Clamped rather than refused, matching the server, which clamps to
        # [1, 256] so a typo cannot cause unbounded dispatch.
        self.max_in_flight = min(self.max_in_flight, env.MAX_IN_FLIGHT_CEILING)


@dataclass(order=True)
class _Outbound:
    """One queued envelope.

    Ordered by (priority, sequence) so heapq never compares the payload — two
    dicts are not orderable, and a tie on priority would raise rather than
    fall back to arrival order.
    """

    priority: int
    seq: int
    body: dict[str, Any] = field(compare=False)


@dataclass
class _Inflight:
    """A running handler, and the generation it was tracked under."""

    generation: int
    cancelled: threading.Event


class _JobCheckpointer:
    """Routes a handler's checkpoint() through the priority queue."""

    def __init__(self, worker: DispatchedWorker, job_id: int) -> None:
        self._worker = worker
        self._job_id = job_id

    def report(self, pct: int, msg: str) -> None:
        self._worker._send_ctrl(env.checkpoint_envelope(self._job_id, pct, msg))


class DispatchedWorker:
    """Drains one queue over one stream until stopped."""

    def __init__(self, channel: grpc.Channel, registry: Registry, config: WorkerConfig) -> None:
        if len(registry) == 0:
            raise ValueError(
                "the registry is empty, so Open would declare no job names and the "
                "server would refuse the session"
            )
        self._channel = channel
        self._registry = registry
        self._cfg = config

        self._stopping = threading.Event()
        # Per instance, not per class. As a class attribute two workers in one
        # process would share one event, and stopping either would stop the
        # other's heartbeat while its leases were still live.
        self._stop_heartbeat = threading.Event()
        self._outbound: queue.PriorityQueue[_Outbound] = queue.PriorityQueue(
            maxsize=config.max_in_flight * 4 + 16
        )
        self._seq = 0
        self._seq_lock = threading.Lock()

        self._inflight: dict[int, _Inflight] = {}
        self._inflight_lock = threading.Lock()
        self._generation = 0

        self._heartbeat_s = 5.0
        self._session_id = ""

    # -- lifecycle ---------------------------------------------------------

    def run(self) -> None:
        """Open the stream and drive it until :meth:`stop`.

        Reconnects with exponential backoff and jitter — 1s doubling to 30s,
        ±25% — so a fleet that loses the server does not return in lockstep
        and knock it over again.
        """
        backoff = _BACKOFF_BASE_S
        while not self._stopping.is_set():
            try:
                self._run_once()
                backoff = _BACKOFF_BASE_S
            except grpc.RpcError as exc:
                if self._stopping.is_set():
                    return
                log.warning("dispatched worker: stream ended: %s", exc)
            except env.ProtocolError as exc:
                # A malformed envelope is not something a reconnect fixes, but
                # it is also not worth ending the process over: the next
                # session may be served by a healthy server.
                log.error("dispatched worker: protocol error: %s", exc)
            if self._stopping.wait(_jitter(backoff)):
                return
            backoff = min(backoff * 2, _BACKOFF_CAP_S)

    def stop(self) -> None:
        """Ask the worker to finish in-flight work and return."""
        self._stopping.set()
        # Wake the sender, which is parked on an empty queue. A _STOP envelope
        # rather than None: PriorityQueue orders with heapq, and None compared
        # against an _Outbound raises TypeError, so a stop while anything was
        # queued failed instead of stopping.
        #
        # Dropped when the queue is full, which is why the sender polls for
        # _stopping as well; this only shortens the common case.
        self._enqueue(_STOP, {})

    # -- one session -------------------------------------------------------

    def _run_once(self) -> None:
        stub: grpc.StreamStreamMultiCallable[frame_pb2.Frame, frame_pb2.Frame]
        stub = self._channel.stream_stream(
            FRAMED_METHOD,
            request_serializer=lambda f: f.SerializeToString(),
            response_deserializer=frame_pb2.Frame.FromString,
        )
        # A fresh queue per session. Envelopes left from a dead stream name
        # job ids the new session has never heard of, and the server closes a
        # session that acks an unknown id.
        outbound: queue.PriorityQueue[_Outbound] = queue.PriorityQueue(
            maxsize=self._cfg.max_in_flight * 4 + 16
        )
        self._outbound = outbound
        self._send_ctrl(
            env.open_session(
                self._cfg.queue,
                self._registry.job_names(),
                self._cfg.max_in_flight,
                self._cfg.pod_id,
                self._cfg.version,
            )
        )

        call = stub(self._outbound_frames(outbound))
        pool = ThreadPoolExecutor(max_workers=self._cfg.max_in_flight)
        heartbeat = threading.Thread(target=self._heartbeat_loop, daemon=True)
        started = False
        try:
            for frame in call:
                message = env.decode_server_envelope(frame.json)
                if isinstance(message, env.SessionAccepted):
                    self._on_accepted(message)
                    if not started:
                        heartbeat.start()
                        started = True
                elif isinstance(message, env.Dispatch):
                    self._on_dispatch(message, pool)
                elif isinstance(message, env.Revoke):
                    self._on_revoke(message)
                elif isinstance(message, env.Goodbye):
                    log.info("dispatched worker: goodbye: %s", message.reason)
                    break
        finally:
            self._stop_heartbeat.set()
            if started:
                heartbeat.join(timeout=2)
            self._stop_heartbeat.clear()
            pool.shutdown(wait=True, cancel_futures=False)

    def _on_accepted(self, accepted: env.SessionAccepted) -> None:
        self._session_id = accepted.session_id
        if accepted.heartbeat_ms > 0:
            self._heartbeat_s = accepted.heartbeat_ms / 1000.0
        log.info(
            "dispatched worker: session %s open on %s (heartbeat %.1fs)",
            accepted.session_id,
            self._cfg.queue,
            self._heartbeat_s,
        )

    def _on_dispatch(self, dispatch: env.Dispatch, pool: ThreadPoolExecutor) -> None:
        handler = self._registry.get(dispatch.job_name)
        if handler is None:
            self._send_data(
                env.fail(
                    dispatch.job_id,
                    f"no handler registered for {dispatch.job_name}",
                    retry=True,
                )
            )
            return

        # Ack before anything else. The server revokes a row whose Ack does
        # not arrive within half the lease, so a slow start reads as a dead
        # worker and the job is handed to someone else while this one runs it.
        self._send_data(env.ack(dispatch.job_id))

        with self._inflight_lock:
            if dispatch.job_id in self._inflight:
                # The server can re-dispatch an id already running here: its
                # lease was reclaimed under a blip and routed back. Starting a
                # second handler would stack two runs of one job and inflate
                # the in-flight count. The Ack above re-arms the server's
                # clock; the original handler keeps going.
                log.warning(
                    "dispatched worker: duplicate dispatch for job %d ignored", dispatch.job_id
                )
                return
            self._generation += 1
            generation = self._generation
            cancelled = threading.Event()
            self._inflight[dispatch.job_id] = _Inflight(generation, cancelled)

        pool.submit(self._run_handler, dispatch, handler, generation, cancelled)

    def _run_handler(
        self,
        dispatch: env.Dispatch,
        handler: Any,
        generation: int,
        cancelled: threading.Event,
    ) -> None:
        # Installed here, on the pool thread that runs the handler. A
        # ContextVar set on the submitting thread is invisible from this one.
        try:
            with use_checkpointer(_JobCheckpointer(self, dispatch.job_id)):
                handler(dispatch.args)
        except Exception as exc:
            log.exception("dispatched worker: job %d failed", dispatch.job_id)
            if not cancelled.is_set():
                self._send_data(env.fail(dispatch.job_id, str(exc), retry=True))
        else:
            if not cancelled.is_set():
                self._send_data(env.complete(dispatch.job_id))
        finally:
            self._untrack(dispatch.job_id, generation)

    def _on_revoke(self, revoke: env.Revoke) -> None:
        """The server pulled a row back. Cancellation is cooperative.

        The handler's own idempotency is the real contract, as in Go: a
        handler that never checks ``cancelled`` runs to completion, and the
        Complete it would send is suppressed because the entry is gone.
        """
        with self._inflight_lock:
            running = self._inflight.pop(revoke.job_id, None)
        if running is not None:
            running.cancelled.set()
        log.info("dispatched worker: job %d revoked: %s", revoke.job_id, revoke.reason)

    def _untrack(self, job_id: int, generation: int) -> None:
        """Drop the in-flight entry, but only if it is still this one's.

        A generation mismatch means the id was revoked and re-dispatched while
        this handler was unwinding. Deleting then would clear the new
        dispatch's entry, and its Revoke would find nothing to cancel.
        """
        with self._inflight_lock:
            running = self._inflight.get(job_id)
            if running is not None and running.generation == generation:
                del self._inflight[job_id]

    # -- outbound ----------------------------------------------------------

    def _outbound_frames(
        self, outbound: queue.PriorityQueue[_Outbound]
    ) -> Iterator[frame_pb2.Frame]:
        """Drain one session's priority queue onto its stream.

        One generator, so gRPC sees a single writer and nothing has to
        serialise concurrent sends.

        The queue is an argument, not ``self._outbound``. gRPC consumes the
        request iterator on its own thread, which stays parked in ``get()``
        after the call ends; reading the attribute would make that generator
        take the *next* session's envelopes and discard them onto a stream
        that is already closed.

        The timeout is what makes stop() reliable — the _STOP envelope is
        dropped if the queue is full, and this loop would otherwise never
        look at _stopping again.
        """
        while True:
            try:
                item = outbound.get(timeout=_DRAIN_POLL_S)
            except queue.Empty:
                if self._stopping.is_set():
                    return
                continue
            if item.priority == _STOP:
                return
            yield frame_pb2.Frame(json=env.encode(item.body))

    def _send_ctrl(self, body: dict[str, Any]) -> None:
        self._enqueue(_CTRL, body)

    def _send_data(self, body: dict[str, Any]) -> None:
        self._enqueue(_DATA, body)

    def _enqueue(self, priority: int, body: dict[str, Any]) -> None:
        """Queue an envelope, dropping rather than blocking when full.

        Blocking here would stall the receive loop that feeds it, and a worker
        that stops reading stops seeing Revoke — the exact revoke-then-
        re-dispatch loop the priority split exists to prevent, reached from
        the other side. A drop is logged loudly: the server's lease expiry
        recovers the row, and silence would leave a job that never completes
        looking like a job that is simply slow.
        """
        with self._seq_lock:
            self._seq += 1
            seq = self._seq
        try:
            self._outbound.put_nowait(_Outbound(priority, seq, body))
        except queue.Full:
            log.error(
                "dispatched worker: outbound queue full, dropped %s. The server's "
                "lease expiry will recover any job this loses.",
                next(iter(body), "envelope"),
            )

    def _heartbeat_loop(self) -> None:
        """Bump every in-flight lease at the server-advised cadence."""
        while not self._stopping.is_set() and not self._stop_heartbeat.is_set():
            if self._stop_heartbeat.wait(self._heartbeat_s):
                return
            with self._inflight_lock:
                ids = sorted(self._inflight)
            if ids:
                self._send_ctrl(env.heartbeat(ids))


def _jitter(seconds: float) -> float:
    """±25% so a reconnecting fleet does not return in lockstep."""
    return seconds * (1.0 + random.uniform(-_BACKOFF_JITTER, _BACKOFF_JITTER))
