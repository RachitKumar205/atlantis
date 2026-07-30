package admin

import (
	"reflect"
	"strings"
	"testing"
	"time"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// These converters were covered by round-trip tests in grpcshim_test.go: the
// shim mapped proto -> Go struct -> proto, so exercising one direction
// exercised both. Deleting the JSON service removed the FromPB halves, and with
// them the only coverage the ToPB halves had — which is the wrong direction to
// lose, because ToPB builds every response the console and tide read.
//
// Asserted directly against expected proto values rather than by round trip.

// planClassToPB decides tide's exit code. cmd/tide/plan.go maps the class to
// 0/1/2 and anything unrecognised to 3, so a wrong value here is a CI gate
// opening or closing incorrectly.
func TestPlanClassToPB(t *testing.T) {
	for _, tc := range []struct {
		in   ClassName
		want adminpb.PlanClass
	}{
		{ClassAdditive, adminpb.PlanClass_PLAN_CLASS_ADDITIVE},
		{ClassBackfill, adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED},
		{ClassBreaking, adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING},
		{ClassUnclean, adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE},
	} {
		if got := planClassToPB(tc.in); got != tc.want {
			t.Errorf("planClassToPB(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	// A class this build does not know must not silently become a valid one.
	// UNSPECIFIED is the only safe answer: tide exits 3 on it rather than
	// treating an unassessable plan as additive.
	if got := planClassToPB(ClassName("something_new")); got != adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED {
		t.Errorf("unknown class mapped to %v, want UNSPECIFIED — an unrecognised class "+
			"must not resolve to one tide will act on", got)
	}
}

// timeToPB's zero-time handling is load-bearing on the console's worker page: a
// zero time means "never happened", and mapping it to a real timestamp would
// render as 1970 rather than as blank.
func TestTimeToPBDistinguishesZeroFromEpoch(t *testing.T) {
	if got := timeToPB(time.Time{}); got != nil {
		t.Errorf("zero time = %v, want nil — a never-set timestamp must not become 1970", got)
	}

	instant := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	got := timeToPB(instant)
	if got == nil {
		t.Fatal("a real instant mapped to nil")
	}
	if !got.AsTime().Equal(instant) {
		t.Errorf("round-tripped to %v, want %v", got.AsTime(), instant)
	}
}

// jobStatusToPB carries a sentinel: ProgressPct is -1 when the handler has
// never reported, and cmd/tide/jobs.go discriminates on `>= 0`. Clamping it to
// 0 would render "0%" for a job that has reported nothing.
func TestJobStatusToPBPreservesProgressSentinel(t *testing.T) {
	got := jobStatusToPB(JobStatus{JobID: "j1", ProgressPct: -1})
	if got.GetProgressPct() != -1 {
		t.Errorf("ProgressPct = %d, want -1 — the sentinel distinguishes "+
			"'never reported' from 'reported 0%%'", got.GetProgressPct())
	}
	if got := jobStatusToPB(JobStatus{JobID: "j1", ProgressPct: 0}); got.GetProgressPct() != 0 {
		t.Errorf("a deliberate 0%% became %d", got.GetProgressPct())
	}
}

// A converter that silently drops a field is the failure mode these round-trip
// tests used to catch. Reflection over a fully-populated input is the cheap
// stand-in: every scalar set on the way in must be non-zero on the way out.
func TestSessionDetailToPBLeavesNoFieldBehind(t *testing.T) {
	in := DispatcherSessionDetail{
		DispatcherSessionSnapshot: DispatcherSessionSnapshot{
			SessionID:       "s1",
			Caller:          "backend",
			Queue:           "default",
			PodID:           "pod-1",
			SDKVersion:      "v1",
			ConnectedAt:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			LastHeartbeatAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
			MaxInFlight:     4,
			InflightCount:   1,
			Dispatched:      9,
			Completed:       7,
			Failed:          1,
			Revoked:         1,
			Drained:         true,
		},
		JobNames: []string{"send_email"},
		Inflight: []DispatcherInflightDetail{{
			JobID: 1, JobName: "send_email",
			DispatchedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), AckReceived: true,
		}},
		Events: []DispatcherEventSnapshot{{
			At: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Kind: "dispatch",
			JobID: 1, JobName: "send_email", Note: "ok",
		}},
	}
	got := sessionDetailToPB(in)
	if got == nil {
		t.Fatal("nil result")
	}
	v := reflect.ValueOf(got).Elem()
	tp := v.Type()
	for i := 0; i < v.NumField(); i++ {
		name := tp.Field(i).Name
		// Skip protobuf's internal bookkeeping fields.
		if strings.HasPrefix(name, "state") || strings.HasPrefix(name, "sizeCache") ||
			strings.HasPrefix(name, "unknownFields") || !v.Field(i).CanInterface() {
			continue
		}
		if v.Field(i).IsZero() {
			t.Errorf("sessionDetailToPB left %s zero even though the input populated "+
				"every source field — the converter is dropping it", name)
		}
	}
}
