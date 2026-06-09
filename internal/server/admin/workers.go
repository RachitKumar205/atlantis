// Admin RPCs that surface + control the worker-poll dispatcher
// (internal/server/jobsdispatcher).
//
// All four RPCs read or mutate the dispatcher's in-memory session
// map. They are wired by cmd/server/main.go via Service.SetDispatcher
// after both the Service and the Dispatcher are constructed — the
// admin package deliberately does NOT import jobsdispatcher (to
// preserve the existing import hierarchy where admin is a leaf), so
// the contract here is a narrow interface.
//
// Authorization:
//
//   - ListConnectedWorkers / GetWorkerSession: admin role at the
//     console BFF layer. Read-only; no operator allowlist check at
//     the gRPC layer.
//   - DrainWorker / EvictWorker: admin role + sudo at the BFF, plus
//     the existing operatorAllowed check (Service.authorizeOperator)
//     so only the console CN can call these via gRPC.
//
// The BFF also writes audit-log rows on Drain / Evict; the gRPC
// layer doesn't (auditing the BFF layer is where the operator's
// session id is available).

package admin

import (
	"context"
	"errors"
	"time"
)

// WorkerDispatcher is the narrow interface the admin Service uses to
// observe and control connected worker sessions. Implemented by
// jobsdispatcher.Dispatcher; tests can supply a fake.
//
// All methods are non-blocking and side-effect-free (Snapshot/Get) or
// return immediately after initiating an async operation (Drain) or
// after the synchronous control op (Evict).
type WorkerDispatcher interface {
	SnapshotSessions() []DispatcherSessionSnapshot
	GetSession(sessionID string) (DispatcherSessionDetail, bool)
	DrainSession(sessionID string) error
	EvictSession(sessionID string) error
}

// DispatcherSessionSnapshot mirrors jobsdispatcher.SessionSnapshot.
// Duplicated here so the admin package's wire shapes don't pull in
// the dispatcher package as a dependency. The cmd/server wiring
// adapts between the two via a thin shim.
type DispatcherSessionSnapshot struct {
	SessionID       string    `json:"session_id"`
	Caller          string    `json:"caller"`
	Queue           string    `json:"queue"`
	PodID           string    `json:"pod_id,omitempty"`
	SDKVersion      string    `json:"sdk_version,omitempty"`
	ConnectedAt     time.Time `json:"connected_at"`
	LastHeartbeatAt time.Time `json:"last_heartbeat_at"`
	MaxInFlight     int       `json:"max_in_flight"`
	InflightCount   int       `json:"inflight_count"`
	Dispatched      int64     `json:"dispatched"`
	Completed       int64     `json:"completed"`
	Failed          int64     `json:"failed"`
	Revoked         int64     `json:"revoked"`
	Drained         bool      `json:"drained,omitempty"`
}

// DispatcherSessionDetail extends Snapshot with the drill-in payload.
type DispatcherSessionDetail struct {
	DispatcherSessionSnapshot
	JobNames []string                   `json:"job_names"`
	Inflight []DispatcherInflightDetail `json:"inflight"`
	Events   []DispatcherEventSnapshot  `json:"events"`
}

type DispatcherInflightDetail struct {
	JobID        int64     `json:"job_id"`
	JobName      string    `json:"job_name"`
	DispatchedAt time.Time `json:"dispatched_at"`
	AckReceived  bool      `json:"ack_received"`
}

type DispatcherEventSnapshot struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"`
	JobID   int64     `json:"job_id,omitempty"`
	JobName string    `json:"job_name,omitempty"`
	Note    string    `json:"note,omitempty"`
}

// Wire request/response types for the 4 RPCs.

type ListConnectedWorkersRequest struct{}

type ListConnectedWorkersResponse struct {
	Sessions []DispatcherSessionSnapshot `json:"sessions"`
}

type GetWorkerSessionRequest struct {
	SessionID string `json:"session_id"`
}

type GetWorkerSessionResponse struct {
	Session DispatcherSessionDetail `json:"session"`
}

type DrainWorkerRequest struct {
	SessionID string `json:"session_id"`
}

type DrainWorkerResponse struct{}

type EvictWorkerRequest struct {
	SessionID string `json:"session_id"`
}

type EvictWorkerResponse struct{}

// ErrWorkerSessionNotFound is returned by GetWorkerSession,
// DrainWorker, EvictWorker when no session matches the supplied id.
// The BFF maps this to a 404.
var ErrWorkerSessionNotFound = errors.New("worker session not found")

// SetDispatcher injects the dispatcher into the Service. main.go
// calls this after constructing both. Nil dispatcher leaves the four
// RPCs returning a clear "dispatcher not enabled" error rather than
// nil-panicking — important for deployments that run the admin
// service with ATL_JOBS_DISPATCHER_ENABLED=false.
func (s *Service) SetDispatcher(d WorkerDispatcher) {
	s.dispatcher = d
}

// ListConnectedWorkers returns every session currently registered
// with the dispatcher.
func (s *Service) ListConnectedWorkers(_ context.Context, _ ListConnectedWorkersRequest) (*ListConnectedWorkersResponse, error) {
	if s.dispatcher == nil {
		return &ListConnectedWorkersResponse{}, nil
	}
	return &ListConnectedWorkersResponse{
		Sessions: s.dispatcher.SnapshotSessions(),
	}, nil
}

// GetWorkerSession returns the per-session detail payload.
func (s *Service) GetWorkerSession(_ context.Context, req GetWorkerSessionRequest) (*GetWorkerSessionResponse, error) {
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	d, ok := s.dispatcher.GetSession(req.SessionID)
	if !ok {
		return nil, ErrWorkerSessionNotFound
	}
	return &GetWorkerSessionResponse{Session: d}, nil
}

// DrainWorker initiates graceful drain on one session. Returns
// success once the drain is requested; the actual close happens
// asynchronously when in-flight reaches zero (or after the
// dispatcher's internal drain cap, whichever first).
//
// Operator-allowlist gated — only the console CN may invoke this
// via gRPC. The BFF layer wraps this with admin-role + sudo.
func (s *Service) DrainWorker(ctx context.Context, req DrainWorkerRequest) (*DrainWorkerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	if err := s.dispatcher.DrainSession(req.SessionID); err != nil {
		return nil, err
	}
	return &DrainWorkerResponse{}, nil
}

// EvictWorker force-closes a session: stop dispatching, send Goodbye
// + Revoke for every in-flight row, release rows back to pending.
//
// Operator-allowlist gated. The BFF wraps this with admin-role +
// sudo.
func (s *Service) EvictWorker(ctx context.Context, req EvictWorkerRequest) (*EvictWorkerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
		return nil, err
	}
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	if err := s.dispatcher.EvictSession(req.SessionID); err != nil {
		return nil, err
	}
	return &EvictWorkerResponse{}, nil
}
