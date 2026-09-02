package admin

import (
	"reflect"
	"strings"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// TestBeginBackfillPlanRequestCarriesNoSQL is the guard for an audit finding
// that was closed structurally rather than by a check.
//
// BeginBackfillPlan used to accept four SQL strings and a field list from the
// caller and execute them, with nothing verifying the statements bore any
// relation to the submitted schema — which made "may apply schema"
// indistinguishable from "may run arbitrary SQL as the atlantis role".
//
// The server now derives every statement from the submitted files. Keeping that
// true means the request must have nowhere to put SQL, so this asserts the
// shape of the message rather than the behaviour of a handler: re-adding such a
// field to the proto fails here, before anyone can wire it up.
//
// It lived in grpcshim_test.go and was nearly lost when that file was deleted
// with the JSON service. It never depended on the shim — it reads the descriptor
// — and it is here now so its survival does not depend on which file a slice's
// tests happened to land in.
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
	want := map[string]bool{
		"caller": true, "plan_id": true, "files": true,
		// Provenance beside the verified caller; none of the three is an input
		// to anything this message authorizes.
		"actor": true, "actor_email": true, "actor_name": true,
	}
	got := map[string]bool{}
	for i := 0; i < md.Fields().Len(); i++ {
		got[string(md.Fields().Get(i).Name())] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("fields = %v, want %v", got, want)
	}
}

// ApplyMigrationRequest carries the same risk for the same reason: its UpSQL
// field was documented as a drift check the server never read, and the server
// re-emits the DDL from the submitted files. A field for caller-supplied SQL
// here would be the same defect on the other RPC.
func TestApplyMigrationRequestCarriesNoSQL(t *testing.T) {
	md := (&adminpb.ApplyMigrationRequest{}).ProtoReflect().Descriptor()
	for i := 0; i < md.Fields().Len(); i++ {
		name := string(md.Fields().Get(i).Name())
		if strings.Contains(strings.ToLower(name), "sql") {
			t.Errorf("ApplyMigrationRequest.%s reintroduces caller-supplied SQL; "+
				"the server re-emits DDL from the submitted files and the checkpoint hash", name)
		}
	}
}
