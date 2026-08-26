// Admin RPCs that surface + control the worker-poll dispatcher
// (internal/server/jobsdispatcher).
//
// All four RPCs read or mutate the dispatcher's in-memory session
// map. They are wired by cmd/server/main.go via Service.SetDispatcher
// after both the Service and the Dispatcher are constructed. admin
// does not import jobsdispatcher, keeping admin a leaf, so the
// contract here is a narrow interface.
//
// Authorization:
//
//   - ListConnectedWorkers / GetWorkerSession: admin role at the
//     console BFF layer, plus CAPABILITY_WORKERS_READ at the gRPC
//     layer. Read-only.
//   - DrainWorker / EvictWorker: admin role + sudo at the BFF, plus
//     CAPABILITY_OPERATOR at the gRPC layer — they act on a worker
//     that belongs to some other caller.
//
// The BFF writes the audit-log rows on Drain and Evict, the gRPC layer
// having no operator session id to record.

package admin

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
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

type ListConnectedWorkersResponse struct {
	Sessions []DispatcherSessionSnapshot `json:"sessions"`
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
// The BFF currently surfaces it as a 502 like any other RPC error — it
// inspects neither the sentinel nor the status code. Mapping it to a 404
// belongs with the wider error-code work.
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
func (s *Service) ListConnectedWorkers(_ context.Context, _ *adminpb.ListConnectedWorkersRequest) (*adminpb.ListConnectedWorkersResponse, error) {
	if s.dispatcher == nil {
		return &adminpb.ListConnectedWorkersResponse{}, nil
	}
	sessions := s.dispatcher.SnapshotSessions()
	out := &adminpb.ListConnectedWorkersResponse{
		Sessions: make([]*adminpb.DispatcherSessionSnapshot, 0, len(sessions)),
	}
	for _, sess := range sessions {
		out.Sessions = append(out.Sessions, sessionSnapshotToPB(sess))
	}
	return out, nil
}

// GetWorkerSession returns the per-session detail payload.
func (s *Service) GetWorkerSession(_ context.Context, req *adminpb.GetWorkerSessionRequest) (*adminpb.GetWorkerSessionResponse, error) {
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	d, ok := s.dispatcher.GetSession(req.GetSessionId())
	if !ok {
		return nil, ErrWorkerSessionNotFound
	}
	return &adminpb.GetWorkerSessionResponse{Session: sessionDetailToPB(d)}, nil
}

// DrainWorker initiates graceful drain on one session. Returns
// success once the drain is requested; the actual close happens
// asynchronously when in-flight reaches zero (or after the
// dispatcher's internal drain cap, whichever first).
//
// Requires CAPABILITY_OPERATOR — the session belongs to some other
// caller. The BFF layer wraps this with admin-role + sudo.
func (s *Service) DrainWorker(ctx context.Context, req *adminpb.DrainWorkerRequest) (*adminpb.DrainWorkerResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	if err := s.dispatcher.DrainSession(req.GetSessionId()); err != nil {
		return nil, err
	}
	return &adminpb.DrainWorkerResponse{}, nil
}

// EvictWorker force-closes a session: stop dispatching, send Goodbye
// + Revoke for every in-flight row, release rows back to pending.
//
// Requires CAPABILITY_OPERATOR. The BFF wraps this with admin-role +
// sudo.
func (s *Service) EvictWorker(ctx context.Context, req *adminpb.EvictWorkerRequest) (*adminpb.EvictWorkerResponse, error) {
	if err := s.guardOperatorTransport(ctx); err != nil {
		return nil, err
	}
	if s.dispatcher == nil {
		return nil, ErrWorkerSessionNotFound
	}
	if err := s.dispatcher.EvictSession(req.GetSessionId()); err != nil {
		return nil, err
	}
	return &adminpb.EvictWorkerResponse{}, nil
}

// The dispatcher is a separate subsystem that deals in domain types; this
// package is the wire boundary, so the translation lives here rather than
// reshaping the dispatcher's interface.
//
// Two asymmetries between the representations have to be preserved in both
// directions.
//
// DispatcherSessionDetail embeds DispatcherSessionSnapshot, so encoding/json
// flattens the snapshot's fields into the detail object. The proto nests it
// under `snapshot` instead, because proto3 has no embedding and a flattened
// copy would duplicate fourteen fields that then have to be kept in step. The
// JSON shim therefore un-nests on the way back out.
//
// A nil timestamp is not the Unix epoch. timestamppb's AsTime maps nil to
// 1970-01-01, whereas the Go zero time marshals as "0001-01-01T00:00:00Z",
// which is what a never-set ConnectedAt carries. The helpers below keep the
// zero value zero in both directions.
//
// Two behaviour changes follow from the proto shapes.
//
// Timestamps are normalised to UTC. The dispatcher stamps with time.Now(),
// which carries the server's local zone, so "2026-07-29T21:55:38+05:30"
// renders as "...T16:25:38Z". A protobuf Timestamp has no zone, so the offset
// cannot be preserved. Consumers parse with offset-aware Date(), so the instant
// is unchanged.
//
// Empty repeated fields marshal as [] rather than null. jobsdispatcher returns
// non-nil slices because a null crashes the session-detail page, and the
// adapter in cmd/server discards that for two of the three. The wire has no
// nil-versus-empty distinction, so every repeated field here is non-nil.

func timeToPB(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func sessionSnapshotToPB(s DispatcherSessionSnapshot) *adminpb.DispatcherSessionSnapshot {
	return &adminpb.DispatcherSessionSnapshot{
		SessionId:       s.SessionID,
		Caller:          s.Caller,
		Queue:           s.Queue,
		PodId:           s.PodID,
		SdkVersion:      s.SDKVersion,
		ConnectedAt:     timeToPB(s.ConnectedAt),
		LastHeartbeatAt: timeToPB(s.LastHeartbeatAt),
		// No range check here, unlike callers.go's COUNT(*): jobsdispatcher
		// clamps max-in-flight to [1, 256] when the session is created, and
		// the in-flight count is bounded by it, so neither can reach int32.
		MaxInFlight:   int32(s.MaxInFlight),
		InflightCount: int32(s.InflightCount),
		Dispatched:    s.Dispatched,
		Completed:     s.Completed,
		Failed:        s.Failed,
		Revoked:       s.Revoked,
		Drained:       s.Drained,
	}
}

func sessionDetailToPB(d DispatcherSessionDetail) *adminpb.DispatcherSessionDetail {
	out := &adminpb.DispatcherSessionDetail{
		Snapshot: sessionSnapshotToPB(d.DispatcherSessionSnapshot),
		JobNames: d.JobNames,
	}
	for _, f := range d.Inflight {
		out.Inflight = append(out.Inflight, &adminpb.DispatcherInflightDetail{
			JobId:        f.JobID,
			JobName:      f.JobName,
			DispatchedAt: timeToPB(f.DispatchedAt),
			AckReceived:  f.AckReceived,
		})
	}
	for _, e := range d.Events {
		out.Events = append(out.Events, &adminpb.DispatcherEventSnapshot{
			At:      timeToPB(e.At),
			Kind:    e.Kind,
			JobId:   e.JobID,
			JobName: e.JobName,
			Note:    e.Note,
		})
	}
	return out
}
