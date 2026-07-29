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
// Operator-allowlist gated — only the console CN may invoke this
// via gRPC. The BFF layer wraps this with admin-role + sudo.
func (s *Service) DrainWorker(ctx context.Context, req *adminpb.DrainWorkerRequest) (*adminpb.DrainWorkerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
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
// Operator-allowlist gated. The BFF wraps this with admin-role +
// sudo.
func (s *Service) EvictWorker(ctx context.Context, req *adminpb.EvictWorkerRequest) (*adminpb.EvictWorkerResponse, error) {
	if err := s.authorizeOperator(ctx); err != nil {
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

// --- Wire conversion ---
//
// The dispatcher is a separate subsystem that deals in domain types; this
// package is the wire boundary, so the translation lives here rather than
// reshaping the dispatcher's interface.
//
// Two asymmetries between the two representations are deliberate and have to
// be preserved in both directions.
//
// DispatcherSessionDetail embeds DispatcherSessionSnapshot, so encoding/json
// flattens the snapshot's fields into the detail object. The proto nests it
// under `snapshot` instead, because proto3 has no embedding and a flattened
// copy would duplicate fourteen fields that then have to be kept in step. The
// JSON shim therefore un-nests on the way back out.
//
// A nil timestamp is not the Unix epoch. timestamppb's AsTime maps nil to
// 1970-01-01, whereas the Go zero time marshals as "0001-01-01T00:00:00Z" —
// which is what a never-set ConnectedAt emitted before this migration. The
// helpers below keep the zero value zero in both directions.
//
// Two behaviour changes are deliberate rather than incidental.
//
// Timestamps are normalised to UTC. The dispatcher stamps with time.Now(),
// which carries the server's local zone, so a response previously rendered
// "2026-07-29T21:55:38+05:30" and now renders the same instant as
// "...T16:25:38Z". Preserving the offset is not possible — a protobuf
// Timestamp has no zone — and it was never desirable: an API response that
// leaks the server's TZ is a latent bug, not a contract. Consumers parse with
// offset-aware Date(), so the instant is unchanged.
//
// Empty repeated fields marshal as [] rather than null. jobsdispatcher goes
// out of its way to return non-nil slices for exactly this reason (see the
// comment on GetSession: "a null crashes the session-detail page"), and the
// adapter in cmd/server then discards that for two of the three. Rather than
// reproduce an inconsistency that a protobuf round trip cannot represent
// anyway — the wire has no nil-versus-empty distinction — every repeated
// field here is non-nil. That also makes this path and the generated one
// agree once the latter is registered.

func timeToPB(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func timeFromPB(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime()
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

func sessionSnapshotFromPB(p *adminpb.DispatcherSessionSnapshot) DispatcherSessionSnapshot {
	return DispatcherSessionSnapshot{
		SessionID:       p.GetSessionId(),
		Caller:          p.GetCaller(),
		Queue:           p.GetQueue(),
		PodID:           p.GetPodId(),
		SDKVersion:      p.GetSdkVersion(),
		ConnectedAt:     timeFromPB(p.GetConnectedAt()),
		LastHeartbeatAt: timeFromPB(p.GetLastHeartbeatAt()),
		MaxInFlight:     int(p.GetMaxInFlight()),
		InflightCount:   int(p.GetInflightCount()),
		Dispatched:      p.GetDispatched(),
		Completed:       p.GetCompleted(),
		Failed:          p.GetFailed(),
		Revoked:         p.GetRevoked(),
		Drained:         p.GetDrained(),
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

// sessionDetailFromPB flattens the nested snapshot back into the embedded
// field, restoring the JSON shape the console reads.
//
// All three slices come back non-nil so they marshal as [] — see the note on
// empty repeated fields above.
func sessionDetailFromPB(p *adminpb.DispatcherSessionDetail) DispatcherSessionDetail {
	jobNames := p.GetJobNames()
	if jobNames == nil {
		jobNames = []string{}
	}
	out := DispatcherSessionDetail{
		DispatcherSessionSnapshot: sessionSnapshotFromPB(p.GetSnapshot()),
		JobNames:                  jobNames,
		Inflight:                  make([]DispatcherInflightDetail, 0, len(p.GetInflight())),
		Events:                    make([]DispatcherEventSnapshot, 0, len(p.GetEvents())),
	}
	for _, f := range p.GetInflight() {
		out.Inflight = append(out.Inflight, DispatcherInflightDetail{
			JobID:        f.GetJobId(),
			JobName:      f.GetJobName(),
			DispatchedAt: timeFromPB(f.GetDispatchedAt()),
			AckReceived:  f.GetAckReceived(),
		})
	}
	for _, e := range p.GetEvents() {
		out.Events = append(out.Events, DispatcherEventSnapshot{
			At:      timeFromPB(e.GetAt()),
			Kind:    e.GetKind(),
			JobID:   e.GetJobId(),
			JobName: e.GetJobName(),
			Note:    e.GetNote(),
		})
	}
	return out
}
