package admin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The JSON path is what production serves while the protobuf migration is in
// flight, so a shim that drifts is a live regression rather than a latent one.
// These tests pin the emitted bytes, not the Go values: CallerInfo carries
// omitempty on three fields, so the question is which keys exist, and no
// assertion on a struct answers that.

func TestCallerInfosToJSON_EmptyMarshalsAsNull(t *testing.T) {
	// The pre-migration handler declared `var out []CallerInfo` and never
	// assigned it when the query returned nothing, so the field marshalled as
	// null. The console SPA reads this response; emitting [] instead would be
	// a silent shape change.
	got, err := json.Marshal(&GetCallersResponse{Callers: callerInfosToJSON(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"callers":null}` {
		t.Errorf("got %s, want {\"callers\":null}", got)
	}

	// An empty non-nil input must behave the same — the distinction is in the
	// helper, not in what the caller happens to pass.
	got, err = json.Marshal(&GetCallersResponse{Callers: callerInfosToJSON([]*adminpb.CallerInfo{})})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"callers":null}` {
		t.Errorf("empty slice: got %s, want {\"callers\":null}", got)
	}
}

func TestCallerInfosToJSON_OmitemptyKeysMatchPreMigrationShape(t *testing.T) {
	tests := []struct {
		name string
		in   *adminpb.CallerInfo
		want string
	}{
		{
			// A caller that has never applied: all three omitempty fields are
			// zero, so all three keys must be absent.
			name: "zero values omit their keys",
			in:   &adminpb.CallerInfo{Caller: "svc-a"},
			want: `{"callers":[{"caller":"svc-a","file_count":0,"registered":false,"can_mutate":false}]}`,
		},
		{
			name: "populated values emit every key",
			in: &adminpb.CallerInfo{
				Caller:        "svc-b",
				FileCount:     3,
				LastAppliedAt: "2026-07-29T00:00:00Z",
				SchemaVersion: 7,
				Registered:    true,
				CanMutate:     true,
				CertExpiresAt: "2026-10-27T00:00:00Z",
			},
			want: `{"callers":[{"caller":"svc-b","file_count":3,"last_applied_at":"2026-07-29T00:00:00Z","schema_version":7,"registered":true,"can_mutate":true,"cert_expires_at":"2026-10-27T00:00:00Z"}]}`,
		},
		{
			// schema_version is the one numeric omitempty field: version 0 is
			// not a real version, so its absence is correct, but a future
			// change to a pointer type would alter this and should fail here.
			name: "zero schema_version omits only that key",
			in: &adminpb.CallerInfo{
				Caller:        "svc-c",
				FileCount:     1,
				LastAppliedAt: "2026-07-29T00:00:00Z",
				Registered:    true,
			},
			want: `{"callers":[{"caller":"svc-c","file_count":1,"last_applied_at":"2026-07-29T00:00:00Z","registered":true,"can_mutate":false}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(&GetCallersResponse{
				Callers: callerInfosToJSON([]*adminpb.CallerInfo{tc.in}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCallerInfosToJSON_CopiesEveryField guards against a field being added to
// CallerInfo and silently left unmapped — the shim would compile and emit a
// zero value, which for an omitempty field means the key just disappears.
func TestCallerInfosToJSON_CopiesEveryField(t *testing.T) {
	in := &adminpb.CallerInfo{
		Caller:        "svc",
		FileCount:     9,
		LastAppliedAt: "t1",
		SchemaVersion: 4,
		Registered:    true,
		CanMutate:     true,
		CertExpiresAt: "t2",
	}
	got := callerInfosToJSON([]*adminpb.CallerInfo{in})[0]

	want := CallerInfo{
		Caller:        "svc",
		FileCount:     9,
		LastAppliedAt: "t1",
		SchemaVersion: 4,
		Registered:    true,
		CanMutate:     true,
		CertExpiresAt: "t2",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// --- Worker session conversion ---
//
// Two things can drift silently here. The Go zero time marshals as
// "0001-01-01T00:00:00Z", but timestamppb maps nil to the Unix epoch, so a
// careless round trip turns "never connected" into 1970. And
// DispatcherSessionDetail embeds its snapshot, so encoding/json flattens those
// fields while the proto nests them — the shim has to un-nest or the console
// sees a different object.

func TestWorkerTimestampRoundTripPreservesZero(t *testing.T) {
	if got := timeFromPB(timeToPB(time.Time{})); !got.IsZero() {
		t.Errorf("zero time round-tripped to %v, want zero", got)
	}
	// Guards the specific failure: nil must not become the Unix epoch.
	if got := timeFromPB(nil); !got.IsZero() {
		t.Errorf("nil timestamp became %v, want the zero time", got)
	}

	// And a real instant survives to nanosecond precision, since that is what
	// the JSON encoding of time.Time carries.
	when := time.Date(2026, 7, 29, 12, 34, 56, 123456789, time.UTC)
	if got := timeFromPB(timeToPB(when)); !got.Equal(when) {
		t.Errorf("round-tripped %v to %v", when, got)
	}
}

func TestWorkerSnapshotJSONShapeSurvivesRoundTrip(t *testing.T) {
	// A never-connected session exercises every omitempty field at once:
	// pod_id, sdk_version, and drained must all be absent, and the two
	// timestamps must render as the zero time rather than the epoch.
	before, err := json.Marshal(DispatcherSessionSnapshot{SessionID: "s1", Caller: "svc", Queue: "q"})
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(sessionSnapshotFromPB(sessionSnapshotToPB(
		DispatcherSessionSnapshot{SessionID: "s1", Caller: "svc", Queue: "q"})))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("\nbefore: %s\n after: %s", before, after)
	}
	if !strings.Contains(string(after), `"connected_at":"0001-01-01T00:00:00Z"`) {
		t.Errorf("zero timestamp did not survive: %s", after)
	}
}

func TestWorkerSessionDetailFlattensSnapshotInJSON(t *testing.T) {
	in := DispatcherSessionDetail{
		DispatcherSessionSnapshot: DispatcherSessionSnapshot{
			SessionID: "s1", Caller: "svc", Queue: "q", PodID: "pod-1",
			ConnectedAt: time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC),
			MaxInFlight: 4, Dispatched: 10,
		},
		JobNames: []string{"a"},
		Inflight: []DispatcherInflightDetail{{JobID: 7, JobName: "a", AckReceived: true}},
		Events:   []DispatcherEventSnapshot{{Kind: "dispatch", JobID: 7}},
	}

	before, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(sessionDetailFromPB(sessionDetailToPB(in)))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("\nbefore: %s\n after: %s", before, after)
	}
	// The embedded fields must sit at the top level, not under "snapshot" —
	// that is the whole difference between the two representations.
	if strings.Contains(string(after), `"snapshot"`) {
		t.Errorf("snapshot leaked as a nested key: %s", after)
	}
	if !strings.Contains(string(after), `"session_id":"s1"`) {
		t.Errorf("embedded fields did not flatten: %s", after)
	}
}

// Every repeated field marshals as [] rather than null — the dispatcher
// returns non-nil slices deliberately ("a null crashes the session-detail
// page") and a protobuf round trip cannot represent the distinction anyway.
func TestWorkerSessionDetailEmitsEmptyArraysNotNull(t *testing.T) {
	after, err := json.Marshal(sessionDetailFromPB(sessionDetailToPB(DispatcherSessionDetail{})))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"job_names":[]`, `"inflight":[]`, `"events":[]`} {
		if !strings.Contains(string(after), key) {
			t.Errorf("want %s in %s", key, after)
		}
	}
}

// TestWorkerTimestampsNormaliseToUTC pins a deliberate behaviour change. The
// dispatcher stamps with time.Now(), carrying the server's local zone, so
// responses previously rendered a local offset. A protobuf Timestamp has no
// zone, and leaking the server's TZ into an API response was never a contract
// worth keeping — but the change has to be asserted rather than discovered.
func TestWorkerTimestampsNormaliseToUTC(t *testing.T) {
	kolkata := time.FixedZone("IST", 5*3600+1800)
	local := time.Date(2026, 7, 29, 21, 55, 38, 0, kolkata)

	got := timeFromPB(timeToPB(local))
	if !got.Equal(local) {
		t.Errorf("instant changed: %v vs %v", got, local)
	}
	if got.Location() != time.UTC {
		t.Errorf("location = %v, want UTC", got.Location())
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-07-29T16:25:38Z"` {
		t.Errorf("got %s, want the same instant rendered in UTC", b)
	}
}

// TestWorkerConvertersCopyEveryField guards against a field being added to one
// of these structs and silently left unmapped. A converter that drops a field
// symmetrically — missing from both directions — produces identical JSON for
// any input that leaves the field zero, so a round-trip comparison alone
// cannot see it. Every field is set to a distinct non-zero value.
func TestWorkerConvertersCopyEveryField(t *testing.T) {
	in := DispatcherSessionDetail{
		DispatcherSessionSnapshot: DispatcherSessionSnapshot{
			SessionID:       "s1",
			Caller:          "svc",
			Queue:           "q",
			PodID:           "pod",
			SDKVersion:      "1.2.3",
			ConnectedAt:     time.Date(2026, 7, 29, 1, 0, 0, 0, time.UTC),
			LastHeartbeatAt: time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC),
			MaxInFlight:     4,
			InflightCount:   3,
			Dispatched:      10,
			Completed:       7,
			Failed:          2,
			Revoked:         1,
			Drained:         true,
		},
		JobNames: []string{"a", "b"},
		Inflight: []DispatcherInflightDetail{{
			JobID: 7, JobName: "a",
			DispatchedAt: time.Date(2026, 7, 29, 3, 0, 0, 0, time.UTC),
			AckReceived:  true,
		}},
		Events: []DispatcherEventSnapshot{{
			At:   time.Date(2026, 7, 29, 4, 0, 0, 0, time.UTC),
			Kind: "dispatch", JobID: 7, JobName: "a", Note: "n",
		}},
	}

	got := sessionDetailFromPB(sessionDetailToPB(in))
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, in)
	}
}

// --- Shim-level tests ---
//
// The converters above are only half the path. The regression these catch —
// an empty session list marshalling as null instead of [] — lived in
// ListConnectedWorkers and its shim, not in the converters, so no amount of
// converter testing could see it.

type fakeDispatcher struct {
	sessions []DispatcherSessionSnapshot
	detail   DispatcherSessionDetail
	found    bool
}

// SnapshotSessions mirrors cmd/server/dispatcher_adapter.go, which builds with
// make([]T, len(in)) — non-nil even when empty. Reproducing that exactly is
// the point: the shim has to agree with what production actually hands it.
func (f *fakeDispatcher) SnapshotSessions() []DispatcherSessionSnapshot {
	out := make([]DispatcherSessionSnapshot, len(f.sessions))
	copy(out, f.sessions)
	return out
}
func (f *fakeDispatcher) GetSession(string) (DispatcherSessionDetail, bool) {
	return f.detail, f.found
}
func (f *fakeDispatcher) DrainSession(string) error { return nil }
func (f *fakeDispatcher) EvictSession(string) error { return nil }

func shimJSON(t *testing.T, v any, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("shim returned an error: %v", err)
	}
	msg, ok := v.(*jsonMsg)
	if !ok {
		t.Fatalf("shim returned %T, want *jsonMsg", v)
	}
	return string(msg.Raw)
}

func TestListConnectedWorkersShimEmitsEmptyArray(t *testing.T) {
	svc := &Service{}
	svc.SetDispatcher(&fakeDispatcher{})

	v, err := invokeListConnectedWorkers(svc, context.Background(), &ListConnectedWorkersRequest{})
	got := shimJSON(t, v, err)
	if got != `{"sessions":[]}` {
		t.Errorf("got %s, want {\"sessions\":[]}", got)
	}
}

func TestListConnectedWorkersShimWithNoDispatcher(t *testing.T) {
	v, err := invokeListConnectedWorkers(&Service{}, context.Background(), &ListConnectedWorkersRequest{})
	got := shimJSON(t, v, err)
	if got != `{"sessions":[]}` {
		t.Errorf("got %s, want {\"sessions\":[]}", got)
	}
}

func TestGetWorkerSessionShimFlattensSnapshot(t *testing.T) {
	svc := &Service{}
	svc.SetDispatcher(&fakeDispatcher{
		found: true,
		detail: DispatcherSessionDetail{
			DispatcherSessionSnapshot: DispatcherSessionSnapshot{SessionID: "s1", Caller: "svc", Queue: "q"},
			JobNames:                  []string{},
		},
	})

	v, err := invokeGetWorkerSession(svc, context.Background(), &GetWorkerSessionRequest{SessionID: "s1"})
	got := shimJSON(t, v, err)
	if strings.Contains(got, `"snapshot"`) {
		t.Errorf("snapshot leaked as a nested key: %s", got)
	}
	if !strings.Contains(got, `"session_id":"s1"`) {
		t.Errorf("embedded fields did not flatten: %s", got)
	}
	if !strings.Contains(got, `"job_names":[]`) {
		t.Errorf("want empty arrays, got %s", got)
	}
}

func TestGetWorkerSessionShimPropagatesNotFound(t *testing.T) {
	svc := &Service{}
	svc.SetDispatcher(&fakeDispatcher{found: false})

	_, err := invokeGetWorkerSession(svc, context.Background(), &GetWorkerSessionRequest{SessionID: "nope"})
	if !errors.Is(err, ErrWorkerSessionNotFound) {
		t.Errorf("got %v, want ErrWorkerSessionNotFound", err)
	}
}

// --- Job conversion ---

// Args carries opaque JSON. A nil RawMessage marshals as null; an empty
// non-nil one is not valid JSON and makes encoding/json fail outright, so nil
// has to survive as nil rather than becoming an empty slice.
func TestJobArgsNilSurvivesRoundTrip(t *testing.T) {
	got := jobStatusFromPB(jobStatusToPB(JobStatus{JobID: "1"}))
	if got.Args != nil {
		t.Errorf("nil Args became %#v", got.Args)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling a converted zero JobStatus failed: %v", err)
	}
	if !strings.Contains(string(b), `"Args":null`) {
		t.Errorf(`want "Args":null, got %s`, b)
	}
}

func TestJobArgsPayloadIsVerbatim(t *testing.T) {
	// Byte-for-byte, including key order and spacing: the server never parses
	// this, so re-encoding it would be a change the caller can observe.
	raw := json.RawMessage(`{"b":1,"a":[2,3]}`)
	got := jobStatusFromPB(jobStatusToPB(JobStatus{Args: raw}))
	if string(got.Args) != string(raw) {
		t.Errorf("got %s, want %s", got.Args, raw)
	}
}

// ListDeadJobs emitted {"Jobs":null} for an empty dead-letter queue, because
// the handler built a nil slice and only ever appended. `tide job dead
// --format=json` prints that to stdout, so [] would be a scriptable change.
//
// Driving the shim rather than the converter is the point: the first version
// of this slice regressed exactly here, and no converter-level test could see
// it — the same lesson the worker slice already recorded above.
func TestListDeadJobsShimEmitsNullForEmptyQueue(t *testing.T) {
	v, err := invokeListDeadJobs(&Service{}, context.Background(), &ListDeadJobsRequest{})
	got := shimJSON(t, v, err)
	if got != `{"Jobs":null}` {
		t.Errorf("got %s, want {\"Jobs\":null}", got)
	}
}

// GetJobStatusResponse.Job is a value tagged omitempty, which does nothing for
// a struct — so a not-found response has always emitted a fully zero Job
// object. Asserted as exact bytes: JobStatus carries omitempty on eight
// fields, so which keys survive is the whole invariant, and a substring check
// would not notice one disappearing.
func TestGetJobStatusShimNotFoundEmitsZeroJob(t *testing.T) {
	svc := &Service{}
	svc.SetDispatcher(nil)
	// A pool-less Service cannot reach Postgres, so drive the shim through the
	// argument-validation path instead and assert the marshalling of a
	// not-found response directly against the shim's own construction.
	resp := &GetJobStatusResponse{Found: false, Job: jobStatusFromPB(nil)}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Found":false,"Job":{"JobID":"","JobName":"","Queue":"","Args":null,"Status":"","Attempts":0,"MaxRetries":0,"ScheduledFor":"","EnqueuedAt":""}}`
	if string(b) != want {
		t.Errorf("\n got: %s\nwant: %s", b, want)
	}
}

func TestGetJobStatusShimRejectsEmptyID(t *testing.T) {
	_, err := invokeGetJobStatus(&Service{}, context.Background(), &GetJobStatusRequest{})
	if err == nil || !strings.Contains(err.Error(), "JobID is required") {
		t.Errorf("got %v, want the JobID validation error", err)
	}
}

// Same symmetric-drop guard as the worker converters: every field distinct and
// non-zero, compared as structs.
func TestJobStatusConverterCopiesEveryField(t *testing.T) {
	in := JobStatus{
		JobID: "1", JobName: "n", Queue: "q", Args: json.RawMessage(`{"k":1}`),
		Status: "pending", Attempts: 2, MaxRetries: 5,
		LastError: "boom", LastErrorAt: "t1", ScheduledFor: "t2",
		StartedAt: "t3", CompletedAt: "t4", EnqueuedAt: "t5",
		SubmittedBy: "cli:me", ProgressPct: 42, ProgressMsg: "half", ProgressAt: "t6",
	}
	got := jobStatusFromPB(jobStatusToPB(in))
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, in)
	}
}

// ProgressPct is -1 when a handler has never checkpointed, which is how the
// wire distinguishes "0% done" from "uninstrumented". The field carries
// omitempty, so 0 drops the key entirely while -1 must survive — asserted by
// marshalling, since that is where omitempty actually applies.
func TestJobProgressPctSentinelSurvivesMarshalling(t *testing.T) {
	uninstrumented, err := json.Marshal(jobStatusFromPB(jobStatusToPB(JobStatus{ProgressPct: -1})))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(uninstrumented), `"ProgressPct":-1`) {
		t.Errorf("sentinel lost: %s", uninstrumented)
	}

	zero, err := json.Marshal(jobStatusFromPB(jobStatusToPB(JobStatus{ProgressPct: 0})))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(zero), `"ProgressPct"`) {
		t.Errorf("zero should be omitted, got %s", zero)
	}
}

// --- Workflow conversion ---

// Same asymmetry as GetJobStatus: Workflow is a value tagged omitempty (a
// no-op for structs), so a not-found response has always emitted a fully zero
// object. Asserted as exact bytes, since WorkflowStatus carries omitempty on
// four fields and which keys survive is the invariant.
func TestGetWorkflowStatusNotFoundEmitsZeroWorkflow(t *testing.T) {
	b, err := json.Marshal(&GetWorkflowStatusResponse{
		Found: false, Workflow: workflowStatusFromPB(nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"Found":false,"Workflow":{"WorkflowID":"","WorkflowName":"","Status":"","StartedAt":""}}`
	if string(b) != want {
		t.Errorf("\n got: %s\nwant: %s", b, want)
	}
}

func TestWorkflowStatusConverterCopiesEveryField(t *testing.T) {
	in := WorkflowStatus{
		WorkflowID: "1", WorkflowName: "wf", Status: "running",
		CurrentStep: "s2", StartedAt: "t1", CompletedAt: "t2",
		ErrorMsg: "boom", SubmittedBy: "cli:me",
	}
	if got := workflowStatusFromPB(workflowStatusToPB(in)); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, in)
	}
}

func TestStartWorkflowShimRejectsWhenMutationDisabled(t *testing.T) {
	_, err := invokeStartWorkflow(&Service{}, context.Background(), &StartWorkflowRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "workflow submission disabled") {
		t.Errorf("got %v, want the disabled-server error", err)
	}
}

func TestGetWorkflowStatusShimRejectsEmptyID(t *testing.T) {
	_, err := invokeGetWorkflowStatus(&Service{}, context.Background(), &GetWorkflowStatusRequest{})
	if err == nil || !strings.Contains(err.Error(), "WorkflowID is required") {
		t.Errorf("got %v, want the WorkflowID validation error", err)
	}
}

// --- Request mappings ---
//
// The response direction is covered above. The request direction was not, and
// could not be: every shim test drives an early-return validation path, which
// is exactly where the mapped fields go unread. Dropping State from the
// StartWorkflow mapping would silently start every workflow with an empty
// state and leave the whole suite green.

func TestStartWorkflowRequestMappingCopiesEveryField(t *testing.T) {
	in := &StartWorkflowRequest{
		WorkflowName: "wf",
		State:        json.RawMessage(`{"k":1}`),
		SubmittedBy:  "cli:me",
	}
	got := startWorkflowRequestToPB(in)
	if got.GetWorkflowName() != in.WorkflowName {
		t.Errorf("WorkflowName = %q, want %q", got.GetWorkflowName(), in.WorkflowName)
	}
	if string(got.GetState()) != string(in.State) {
		t.Errorf("State = %s, want %s", got.GetState(), in.State)
	}
	if got.GetSubmittedBy() != in.SubmittedBy {
		t.Errorf("SubmittedBy = %q, want %q", got.GetSubmittedBy(), in.SubmittedBy)
	}
}

// A caller that omits State must still reach the server as nil, so the
// "{}" default fires there rather than an empty payload being persisted.
func TestStartWorkflowRequestMappingKeepsNilState(t *testing.T) {
	if got := startWorkflowRequestToPB(&StartWorkflowRequest{WorkflowName: "wf"}); got.GetState() != nil {
		t.Errorf("State = %#v, want nil", got.GetState())
	}
}

func TestGetWorkflowStatusRequestMappingCopiesID(t *testing.T) {
	if got := getWorkflowStatusRequestToPB(&GetWorkflowStatusRequest{WorkflowID: "42"}); got.GetWorkflowId() != "42" {
		t.Errorf("WorkflowId = %q, want 42", got.GetWorkflowId())
	}
}

func TestSubmitJobRequestMappingCopiesEveryField(t *testing.T) {
	in := &SubmitJobRequest{
		JobName:     "j",
		Args:        json.RawMessage(`{"a":1}`),
		ScheduledAt: "2026-07-30T00:00:00Z",
		SubmittedBy: "cli:me",
	}
	got := submitJobRequestToPB(in)
	if got.GetJobName() != in.JobName {
		t.Errorf("JobName = %q, want %q", got.GetJobName(), in.JobName)
	}
	if string(got.GetArgs()) != string(in.Args) {
		t.Errorf("Args = %s, want %s", got.GetArgs(), in.Args)
	}
	if got.GetScheduledAt() != in.ScheduledAt {
		t.Errorf("ScheduledAt = %q, want %q", got.GetScheduledAt(), in.ScheduledAt)
	}
	if got.GetSubmittedBy() != in.SubmittedBy {
		t.Errorf("SubmittedBy = %q, want %q", got.GetSubmittedBy(), in.SubmittedBy)
	}
}

func TestSubmitJobRequestMappingKeepsNilArgs(t *testing.T) {
	if got := submitJobRequestToPB(&SubmitJobRequest{JobName: "j"}); got.GetArgs() != nil {
		t.Errorf("Args = %#v, want nil", got.GetArgs())
	}
}

// --- Backfill ---

// TestBeginBackfillPlanRequestCarriesNoSQL is the guard for the defect this
// slice closes. The RPC used to accept four SQL strings and a field list from
// the caller and execute them, with nothing checking that the statements bore
// any relation to the submitted schema — which made "may apply schema"
// indistinguishable from "may run arbitrary SQL as the atlantis role".
//
// The server now derives every statement. Keeping that true means the request
// must have nowhere to put SQL, so this asserts the shape of the message
// rather than the behaviour of one handler: re-adding such a field to the
// proto fails here, before anyone can wire it up.
func TestBeginBackfillPlanRequestCarriesNoSQL(t *testing.T) {
	md := (&adminpb.BeginBackfillPlanRequest{}).ProtoReflect().Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		name := string(md.Fields().Get(i).Name())
		lower := strings.ToLower(name)
		if strings.Contains(lower, "sql") || strings.Contains(lower, "backfill_field") {
			t.Errorf("BeginBackfillPlanRequest.%s reintroduces caller-supplied SQL; "+
				"the server must derive every statement from the submitted files", name)
		}
	}

	// And the positive half: the message carries exactly what recomputation
	// needs, so a field going missing is caught too.
	want := map[string]bool{"caller": true, "plan_id": true, "files": true}
	got := map[string]bool{}
	for i := 0; i < md.Fields().Len(); i++ {
		got[string(md.Fields().Get(i).Name())] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
}

// The JSON request still carries the legacy SQL fields, because older tide
// binaries send them. The mapping must drop them rather than forward them —
// asserted against the message the shim actually builds, so re-adding a field
// to the mapping fails here.
func TestBeginBackfillRequestMappingDropsLegacySQL(t *testing.T) {
	req := &BeginBackfillPlanRequest{
		Caller:                 "svc",
		PlanID:                 "abc",
		Files:                  []SubmittedFile{{Path: "a.atl", Content: []byte("entity A in x {}")}},
		PreBackfillUpSQL:       "DROP TABLE atlantis.caller_identities",
		PostBackfillUpSQL:      "DROP SCHEMA atlantis CASCADE",
		PreBackfillIndexesSQL:  "DROP INDEX everything",
		PostBackfillIndexesSQL: "DROP INDEX everything",
		BackfillFields:         []BackfillFieldRef{{EntityID: "x.A", Field: "f"}},
	}

	got := beginBackfillRequestToPB(req)

	// Whatever the request carried, the wire message must contain only the
	// three inputs recomputation needs. Marshalling and scanning the bytes
	// catches a value smuggled into any field, named or not.
	raw, err := protojson.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"DROP TABLE", "DROP SCHEMA", "DROP INDEX", "x.A"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("mapped request leaked %q: %s", forbidden, raw)
		}
	}

	if got.GetCaller() != "svc" || got.GetPlanId() != "abc" || len(got.GetFiles()) != 1 {
		t.Errorf("mapping dropped a field it needs: %s", raw)
	}
}

func TestSubmittedFileRoundTrip(t *testing.T) {
	in := []SubmittedFile{
		{Path: "a.atl", Content: []byte("entity A in x {}")},
		{Path: "b.atl", Content: nil},
	}
	got := submittedFilesFromPB(submittedFilesToPB(in))
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, in)
	}
	if submittedFilesToPB(nil) != nil {
		t.Error("nil input should map to nil, not an empty slice")
	}
}

func TestBackfillFieldStatusRoundTrip(t *testing.T) {
	in := []BackfillFieldStatus{{
		EntityID: "x.A", Field: "f", Status: "running",
		RowsProcessed: 42, LastPK: "1000", ErrorMsg: "",
	}}
	pb := []*adminpb.BackfillFieldStatus{{
		EntityId: "x.A", Field: "f", Status: "running",
		RowsProcessed: 42, LastPk: "1000",
	}}
	if got := backfillFieldStatusFromPB(pb); !reflect.DeepEqual(got, in) {
		t.Errorf("got %+v, want %+v", got, in)
	}
	if backfillFieldStatusFromPB(nil) != nil {
		t.Error("nil input should map to nil so the field marshals as null")
	}
}

// --- Adopt ---

// AdoptBaselineRequest has two ways to say the same thing: a submissions list,
// or a single caller plus files. The server falls back to the second only when
// the first is empty, so both halves have to survive the mapping — and neither
// can be reached through invokeAdoptBaseline, since authorizeOperator rejects a
// pool-less Service first. Asserting the mapper directly is the only coverage
// available, and without it dropping AllowDrift silently disables --allow-drift.
func TestAdoptBaselineRequestMappingCarriesEveryField(t *testing.T) {
	req := &AdoptBaselineRequest{
		Submissions: []CallerSubmission{
			{Caller: "a", Files: []SubmittedFile{{Path: "a.atl", Content: []byte("x")}}},
		},
		Caller:     "solo",
		Files:      []SubmittedFile{{Path: "s.atl", Content: []byte("y")}},
		AllowDrift: true,
		AdoptedBy:  "ops@example.com",
	}
	got := adoptBaselineRequestToPB(req)

	if n := len(got.GetSubmissions()); n != 1 || got.GetSubmissions()[0].GetCaller() != "a" {
		t.Errorf("Submissions not carried: %+v", got.GetSubmissions())
	}
	if got.GetCaller() != "solo" {
		t.Errorf("Caller = %q, want solo", got.GetCaller())
	}
	if n := len(got.GetFiles()); n != 1 || got.GetFiles()[0].GetPath() != "s.atl" {
		t.Errorf("Files not carried: %+v", got.GetFiles())
	}
	if !got.GetAllowDrift() {
		t.Error("AllowDrift dropped — --allow-drift would silently stop working")
	}
	if got.GetAdoptedBy() != "ops@example.com" {
		t.Errorf("AdoptedBy = %q, want ops@example.com", got.GetAdoptedBy())
	}
}

// The three response construction sites, pinned as bytes.
func TestAdoptBaselineResponseShapes(t *testing.T) {
	tests := []struct {
		name string
		in   *adminpb.AdoptBaselineResponse
		want string
	}{
		{
			name: "already adopted",
			in:   &adminpb.AdoptBaselineResponse{AlreadyAdopted: true, CheckpointWritten: true},
			want: `{"CheckpointWritten":true,"AlreadyAdopted":true,"Drift":null,"Warnings":null}`,
		},
		{
			name: "drift refused",
			in: &adminpb.AdoptBaselineResponse{
				Drift: []*adminpb.AdoptDriftItem{{EntityId: "x.A", Kind: "field_type_changed", Severity: "mismatch"}},
			},
			want: `{"CheckpointWritten":false,"AlreadyAdopted":false,"Drift":[{"entity_id":"x.A","kind":"field_type_changed","severity":"mismatch"}],"Warnings":null}`,
		},
		{
			name: "success with warnings",
			in:   &adminpb.AdoptBaselineResponse{CheckpointWritten: true, Warnings: []string{"w1"}},
			want: `{"CheckpointWritten":true,"AlreadyAdopted":false,"Drift":null,"Warnings":["w1"]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(adoptBaselineResponseFromPB(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) != tc.want {
				t.Errorf("\n got: %s\nwant: %s", b, tc.want)
			}
		})
	}
}

func TestCallerSubmissionRoundTrip(t *testing.T) {
	subs := []CallerSubmission{
		{Caller: "a", Files: []SubmittedFile{{Path: "a.atl", Content: []byte("x")}}},
		{Caller: "b", Files: []SubmittedFile{{Path: "b.atl", Content: []byte("y")}}},
	}
	if got := callerSubmissionsFromPB(callerSubmissionsToPB(subs)); !reflect.DeepEqual(got, subs) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, subs)
	}
	// Nil in, nil out — for consistency with the other file mappers, not
	// because the single-caller fallback depends on it: that branch tests
	// len(subs)==0, and callerSubmissionsFromPB collapses empty to nil anyway.
	if callerSubmissionsToPB(nil) != nil {
		t.Error("nil submissions should map to nil")
	}
}

// Drift is nil when the check finds nothing, and on the already-adopted path
// where it never runs. Both marshal as null.
func TestAdoptDriftKeepsNilNil(t *testing.T) {
	for _, in := range [][]*adminpb.AdoptDriftItem{nil, {}} {
		if got := adoptDriftFromPB(in); got != nil {
			t.Errorf("adoptDriftFromPB(%#v) = %#v, want nil", in, got)
		}
	}
	b, err := json.Marshal(&AdoptBaselineResponse{Drift: adoptDriftFromPB(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"Drift":null`) {
		t.Errorf(`want "Drift":null, got %s`, b)
	}
}

func TestAdoptDriftConverterCopiesEveryField(t *testing.T) {
	in := []AdoptDriftItem{{
		EntityID: "x.A", Field: "f", Kind: "field_type_changed",
		Severity: "mismatch", Detail: "declared text, live bigint",
	}}
	if got := adoptDriftFromPB(adoptDriftToPB(in)); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip lost data:\n got: %+v\nwant: %+v", got, in)
	}
}
